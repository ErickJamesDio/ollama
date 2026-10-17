//go:build windows || darwin

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
)

const (
	contextCompactionTriggerRatio = 0.75
	contextCompactionTargetRatio  = 0.35
	contextCompactionChunkRatio   = 0.75
	contextCompactionMaxOutput    = 4096
	contextRecoveryPreviewBytes   = 4096
	contextRecoveryMaxChunkBytes  = 65536
	contextRequestOverheadTokens  = 256
	contextRecoveryToolName       = "read_tool_result_chunk"
)

type chatClient interface {
	Chat(context.Context, *api.ChatRequest, api.ChatResponseFunc) error
}

func (s *Server) operationalChatForRequest(ctx context.Context, c chatClient, chat *store.Chat, model string) (*store.Chat, error) {
	return s.operationalChatForRequestWithTraceAndOverhead(ctx, c, chat, model, 0, nil)
}

type contextCompactionTrace struct {
	Limit                  int
	CanonicalTurns         int
	CanonicalEstimate      int
	CheckpointCoveredTurns int
	ProjectedTurns         int
	ProjectedEstimate      int
	Triggered              bool
	Coverage               int
	DeltaTurns             int
	BeforeTokens           int
	AfterTokens            int
	SummaryTokens          int
	SummaryBytes           int
	OutputTurns            int
	OutputEstimate         int
	Fallback               string
	Error                  string
}

func (s *Server) operationalChatForRequestWithTrace(ctx context.Context, c chatClient, chat *store.Chat, model string, trace *contextCompactionTrace) (*store.Chat, error) {
	return s.operationalChatForRequestWithTraceAndOverhead(ctx, c, chat, model, 0, trace)
}

func (s *Server) operationalChatForRequestWithTraceAndOverhead(ctx context.Context, c chatClient, chat *store.Chat, model string, requestOverheadTokens int, trace *contextCompactionTrace) (*store.Chat, error) {
	if trace != nil {
		trace.CanonicalTurns = len(chat.Messages)
	}
	settings, err := s.Store.Settings()
	if err != nil {
		s.log().Warn("context compaction skipped: settings unavailable", "chat_id", chat.ID, "error", err)
		if trace != nil {
			trace.Fallback = "settings_unavailable"
			trace.Error = err.Error()
		}
		return chat, nil
	}
	limit := settings.ContextLength
	if trace != nil {
		trace.Limit = limit
	}
	if limit <= 0 {
		if trace != nil {
			trace.Fallback = "disabled"
		}
		return chat, nil
	}
	requestBudget := contextInputBudget(limit, requestOverheadTokens)

	checkpoint, err := s.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		s.log().Warn("context compaction skipped: checkpoint unavailable", "chat_id", chat.ID, "error", err)
		if trace != nil {
			trace.Fallback = "checkpoint_unavailable"
			trace.Error = err.Error()
		}
		return chat, nil
	}

	currentEstimate := estimateMessagesTokens(chat.Messages)
	if trace != nil {
		trace.CanonicalEstimate = currentEstimate
	}
	trigger := min(int(float64(limit)*contextCompactionTriggerRatio), requestBudget)
	requestProjection := projectChatForRequest(chat, checkpoint, requestBudget)
	requestProjectionEstimate := estimateMessagesTokens(requestProjection.Messages)
	if checkpoint != nil {
		if trace != nil {
			trace.CheckpointCoveredTurns = checkpoint.CoveredMessageCount
		}
		projected := requestProjection
		projectedEstimate := estimateMessagesTokens(projected.Messages)
		if trace != nil {
			trace.ProjectedTurns = len(projected.Messages)
			trace.ProjectedEstimate = projectedEstimate
		}
		if projectedEstimate < trigger {
			s.log().Info("context compaction projection active", "chat_id", chat.ID, "covered", checkpoint.CoveredMessageCount, "estimate", projectedEstimate, "canonical_estimate", currentEstimate, "limit", limit)
			if trace != nil {
				trace.Fallback = "checkpoint_projection"
				trace.OutputTurns = len(projected.Messages)
				trace.OutputEstimate = projectedEstimate
			}
			return projected, nil
		}
	}
	if checkpoint == nil && requestProjectionEstimate < trigger {
		if trace != nil {
			trace.ProjectedTurns = len(requestProjection.Messages)
			trace.ProjectedEstimate = requestProjectionEstimate
			trace.Fallback = "request_projection"
			trace.OutputTurns = len(requestProjection.Messages)
			trace.OutputEstimate = requestProjectionEstimate
		}
		return requestProjection, nil
	}

	if currentEstimate < trigger {
		if trace != nil {
			trace.Fallback = "below_trigger"
			trace.OutputTurns = len(chat.Messages)
			trace.OutputEstimate = currentEstimate
		}
		return chat, nil
	}

	coverage := chooseContextCoverage(chat.Messages, int(float64(limit)*contextCompactionTargetRatio))
	if pendingStart := pendingToolChainStart(chat.Messages); pendingStart >= 0 && coverage > pendingStart {
		coverage = pendingStart
	}
	if coverage <= 0 {
		if trace != nil {
			trace.Fallback = "no_safe_coverage"
			trace.OutputTurns = len(chat.Messages)
			trace.OutputEstimate = currentEstimate
		}
		return chat, nil
	}

	oldCovered := 0
	if checkpoint != nil {
		oldCovered = checkpoint.CoveredMessageCount
		if coverage < oldCovered {
			coverage = oldCovered
		}
	}
	if trace != nil {
		trace.Coverage = coverage
		trace.BeforeTokens = currentEstimate
	}
	if coverage <= oldCovered {
		projected := projectChatForRequest(chat, checkpoint, requestBudget)
		if trace != nil {
			trace.Fallback = "checkpoint_projection"
			trace.OutputTurns = len(projected.Messages)
			trace.OutputEstimate = estimateMessagesTokens(projected.Messages)
		}
		return projected, nil
	}
	if trace != nil {
		trace.Triggered = true
	}

	workingCheckpoint := checkpoint
	maxChunkTokens := int(float64(limit) * contextCompactionChunkRatio)
	for oldCovered < coverage {
		nextCoverage := chooseContextCompactionChunk(chat.Messages, oldCovered, coverage, workingCheckpoint, maxChunkTokens)
		if nextCoverage <= oldCovered {
			err := fmt.Errorf("next compaction chunk would exceed configured limit")
			if trace != nil {
				trace.Fallback = "chunk_too_large"
				trace.Error = err.Error()
			}
			return nil, fmt.Errorf("context compaction failed while canonical context estimate %d exceeds configured limit %d: %w", currentEstimate, limit, err)
		}

		delta := chat.Messages[oldCovered:nextCoverage]
		if trace != nil {
			trace.DeltaTurns += len(delta)
		}
		content, err := s.generateContextCheckpoint(ctx, c, model, workingCheckpoint, delta, oldCovered, maxChunkTokens)
		if err != nil || strings.TrimSpace(content) == "" {
			if err == nil {
				err = fmt.Errorf("empty context checkpoint")
			}
			if trace != nil {
				trace.Fallback = "generation_failed"
				trace.Error = err.Error()
			}
			if currentEstimate >= limit {
				return nil, fmt.Errorf("context compaction failed while canonical context estimate %d exceeds configured limit %d: %w", currentEstimate, limit, err)
			}
			s.log().Warn("context compaction generation failed; using canonical history", "chat_id", chat.ID, "error", err)
			if trace != nil {
				trace.OutputTurns = len(chat.Messages)
				trace.OutputEstimate = currentEstimate
			}
			return chat, nil
		}

		summaryTokens := estimateTokens(content)
		newCheckpoint := store.ContextCheckpoint{
			ChatID:                chat.ID,
			CoveredMessageCount:   nextCoverage,
			CoveredFingerprint:    store.MessagePrefixFingerprint(chat.Messages, nextCoverage),
			Content:               strings.TrimSpace(content),
			EstimatedTokensBefore: currentEstimate,
			CreatedAt:             time.Now(),
		}
		newCheckpoint.EstimatedTokensAfter = estimateMessagesTokens(projectChatForRequest(chat, &newCheckpoint, requestBudget).Messages)
		if trace != nil {
			trace.SummaryTokens = summaryTokens
			trace.SummaryBytes = len(content)
			trace.AfterTokens = newCheckpoint.EstimatedTokensAfter
		}
		if err := s.Store.SaveContextCheckpoint(newCheckpoint); err != nil {
			if trace != nil {
				trace.Fallback = "save_failed"
				trace.Error = err.Error()
			}
			if currentEstimate >= limit {
				return nil, fmt.Errorf("context compaction checkpoint save failed while canonical context estimate %d exceeds configured limit %d: %w", currentEstimate, limit, err)
			}
			s.log().Warn("context compaction checkpoint save failed; using canonical history", "chat_id", chat.ID, "error", err)
			if trace != nil {
				trace.OutputTurns = len(chat.Messages)
				trace.OutputEstimate = currentEstimate
			}
			return chat, nil
		}

		workingCheckpoint = &newCheckpoint
		oldCovered = nextCoverage
	}

	if workingCheckpoint == nil {
		return chat, nil
	}

	s.log().Info("context compaction checkpoint created", "chat_id", chat.ID, "trigger_estimate", currentEstimate, "limit", limit, "covered", workingCheckpoint.CoveredMessageCount, "active_estimate", workingCheckpoint.EstimatedTokensAfter)
	projected := projectChatForRequest(chat, workingCheckpoint, requestBudget)
	if trace != nil {
		trace.Fallback = "compacted"
		trace.OutputTurns = len(projected.Messages)
		trace.OutputEstimate = estimateMessagesTokens(projected.Messages)
	}
	return projected, nil
}

func projectCheckpointChat(chat *store.Chat, checkpoint *store.ContextCheckpoint) *store.Chat {
	if checkpoint == nil || checkpoint.CoveredMessageCount <= 0 || checkpoint.CoveredMessageCount > len(chat.Messages) {
		return chat
	}
	projected := *chat
	msgs := make([]store.Message, 0, 1+len(chat.Messages)-checkpoint.CoveredMessageCount)
	msgs = append(msgs, store.NewMessage("system", "Compacted prior conversation state. This is a semantic checkpoint generated from earlier canonical chat messages; it is not a user message and does not replace the full saved conversation.\n\n"+checkpoint.Content, nil))
	msgs = append(msgs, chat.Messages[checkpoint.CoveredMessageCount:]...)
	projected.Messages = msgs
	return &projected
}

// projectChatForRequest creates an ephemeral runner view. The canonical chat is
// never mutated: private reasoning and operational tool metadata are omitted,
// while oversized tool results become explicitly partial, recoverable views.
func projectChatForRequest(chat *store.Chat, checkpoint *store.ContextCheckpoint, budget int) *store.Chat {
	base := projectCheckpointChat(chat, checkpoint)
	projected := *base
	projected.Messages = make([]store.Message, 0, len(base.Messages))
	canonicalIndices := make([]int, 0, len(base.Messages))
	changed := base != chat

	perResultBudget := max(1024, budget/4)
	for i, original := range base.Messages {
		msg := original
		msg.ToolCalls = append([]store.ToolCall(nil), original.ToolCalls...)
		if msg.Thinking != "" || msg.Model != "" || msg.ToolResult != nil || msg.ThinkingTimeStart != nil || msg.ThinkingTimeEnd != nil {
			changed = true
		}
		msg.Thinking = ""
		msg.Model = ""
		msg.ToolResult = nil
		msg.ThinkingTimeStart = nil
		msg.ThinkingTimeEnd = nil
		for j := range msg.ToolCalls {
			if msg.ToolCalls[j].Function.Arguments != "{}" || msg.ToolCalls[j].Function.Result != nil {
				changed = true
			}
			msg.ToolCalls[j].Function.Arguments = "{}"
			msg.ToolCalls[j].Function.Result = nil
		}
		if msg.Role == "tool" && estimateTokens(msg.Content) > perResultBudget {
			changed = true
			canonicalIndex := projectedCanonicalIndex(checkpoint, i)
			msg.Content = toolResultRecoveryPreview(canonicalIndex, msg.ToolName, msg.Content)
		}
		projected.Messages = append(projected.Messages, msg)
		canonicalIndices = append(canonicalIndices, projectedCanonicalIndex(checkpoint, i))
	}
	for estimateMessagesTokens(projected.Messages) > budget {
		largest := -1
		largestTokens := 0
		for i, msg := range projected.Messages {
			if msg.Role != "tool" || strings.Contains(msg.Content, "result_ref=canonical-tool-result:") {
				continue
			}
			if tokens := estimateTokens(msg.Content); tokens > largestTokens {
				largest = i
				largestTokens = tokens
			}
		}
		if largest < 0 || largestTokens <= estimateTokens(projected.Messages[largest].Content[:safeUTF8End(projected.Messages[largest].Content, min(len(projected.Messages[largest].Content), contextRecoveryPreviewBytes))]) {
			break
		}
		changed = true
		msg := &projected.Messages[largest]
		msg.Content = toolResultRecoveryPreview(canonicalIndices[largest], msg.ToolName, base.Messages[largest].Content)
	}
	if !changed {
		return base
	}
	return &projected
}

func projectedCanonicalIndex(checkpoint *store.ContextCheckpoint, projectedIndex int) int {
	if checkpoint == nil || checkpoint.CoveredMessageCount <= 0 {
		return projectedIndex
	}
	if projectedIndex == 0 {
		return -1
	}
	return checkpoint.CoveredMessageCount + projectedIndex - 1
}

func toolResultRef(messageIndex int) string {
	return "canonical-tool-result:" + strconv.Itoa(messageIndex)
}

func toolResultRecoveryPreview(messageIndex int, toolName, content string) string {
	end := safeUTF8End(content, min(len(content), contextRecoveryPreviewBytes))
	return fmt.Sprintf("Tool result is stored canonically but is too large for the active context. Only bytes [0:%d) of %d are included below. result_ref=%s tool=%s. Use %s with result_ref and next_offset=%d to read further parts. Do not claim the complete result was analyzed until all required ranges have been retrieved.\n\n%s",
		end, len(content), toolResultRef(messageIndex), toolName, contextRecoveryToolName, end, content[:end])
}

func safeUTF8End(value string, end int) int {
	end = min(max(end, 0), len(value))
	for end > 0 && end < len(value) && !utf8.RuneStart(value[end]) {
		end--
	}
	return end
}

func pendingToolChainStart(messages []store.Message) int {
	if len(messages) == 0 || messages[len(messages)-1].Role != "tool" {
		return -1
	}
	i := len(messages) - 1
	for i >= 0 && messages[i].Role == "tool" {
		i--
	}
	if i >= 0 && messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
		return i
	}
	return -1
}

func contextInputBudget(limit, requestOverheadTokens int) int {
	return max(1, contextFinalRequestBudget(limit)-requestOverheadTokens-contextRequestOverheadTokens)
}

func contextFinalRequestBudget(limit int) int {
	responseReserve := max(1024, min(contextCompactionMaxOutput, limit/8))
	return max(1, limit-responseReserve)
}

func estimateToolDefinitionsTokens(definitions []map[string]any) int {
	data, err := json.Marshal(definitions)
	if err != nil {
		return 0
	}
	return estimateTokens(string(data)) + len(definitions)*16
}

type toolResultChunkReader struct {
	chat     *store.Chat
	maxBytes int
}

func newToolResultChunkReader(chat *store.Chat, maxBytes int) *toolResultChunkReader {
	return &toolResultChunkReader{chat: chat, maxBytes: min(contextRecoveryMaxChunkBytes, max(4096, maxBytes))}
}

func (r *toolResultChunkReader) Name() string { return contextRecoveryToolName }
func (r *toolResultChunkReader) Description() string {
	return "Read a byte range from an oversized tool result retained in the canonical chat. Use the returned next_offset to continue; complete is true only after the final range."
}
func (r *toolResultChunkReader) Prompt() string { return "" }
func (r *toolResultChunkReader) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"result_ref": map[string]any{"type": "string"},
			"offset":     map[string]any{"type": "integer", "minimum": 0},
			"max_chars":  map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"result_ref", "offset"},
	}
}
func (r *toolResultChunkReader) Execute(_ context.Context, args map[string]any) (any, string, error) {
	ref, _ := args["result_ref"].(string)
	prefix := "canonical-tool-result:"
	index, parseErr := strconv.Atoi(strings.TrimPrefix(ref, prefix))
	if parseErr != nil || !strings.HasPrefix(ref, prefix) || index < 0 || index >= len(r.chat.Messages) || r.chat.Messages[index].Role != "tool" {
		return nil, "", fmt.Errorf("unknown or unavailable tool result reference %q", ref)
	}
	content := r.chat.Messages[index].Content
	offset, err := integerToolArgument(args["offset"])
	if err != nil || offset < 0 || offset > len(content) {
		return nil, "", fmt.Errorf("invalid offset for %s", ref)
	}
	want := r.maxBytes
	if value, exists := args["max_chars"]; exists {
		requested, parseErr := integerToolArgument(value)
		if parseErr != nil || requested <= 0 {
			return nil, "", fmt.Errorf("invalid max_chars for %s", ref)
		}
		want = min(want, requested)
	}
	end := safeUTF8End(content, min(len(content), offset+want))
	if end < offset {
		end = offset
	}
	complete := end >= len(content)
	result := map[string]any{"result_ref": ref, "offset": offset, "next_offset": end, "total_chars": len(content), "complete": complete, "content": content[offset:end]}
	text := fmt.Sprintf("Tool result chunk %s bytes [%d:%d) of %d; complete=%t; next_offset=%d.\n\n%s", ref, offset, end, len(content), complete, end, content[offset:end])
	return result, text, nil
}

func integerToolArgument(value any) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	case json.Number:
		i, err := v.Int64()
		return int(i), err
	default:
		return 0, fmt.Errorf("not an integer")
	}
}

func registerToolResultRecovery(registry *tools.Registry, chat *store.Chat, contextLength int) {
	for _, msg := range chat.Messages {
		if msg.Role == "tool" && estimateTokens(msg.Content) > 1024 {
			registry.Register(newToolResultChunkReader(chat, contextLength*4/8))
			return
		}
	}
}

func estimateAPIChatRequestTokens(req *api.ChatRequest) int {
	total := contextRequestOverheadTokens
	for _, msg := range req.Messages {
		total += estimateTokens(msg.Role) + estimateTokens(msg.Content) + estimateTokens(msg.Thinking) + 12
		for _, image := range msg.Images {
			_ = image
			total += api.ImageTokenEstimate
		}
		for _, call := range msg.ToolCalls {
			encoded, err := json.Marshal(call)
			if err == nil {
				total += estimateTokens(string(encoded))
			}
			total += 16
		}
		total += estimateTokens(msg.ToolName) + estimateTokens(msg.ToolCallID)
	}
	if encoded, err := json.Marshal(req.Tools); err == nil {
		total += estimateTokens(string(encoded)) + len(req.Tools)*16
	}
	return total
}

func chooseContextCoverage(messages []store.Message, targetTailTokens int) int {
	if len(messages) < 4 {
		return 0
	}
	tailTokens := 0
	coverage := len(messages) - 1
	for coverage >= 0 {
		tailTokens += estimateMessageTokens(messages[coverage])
		if tailTokens >= targetTailTokens {
			break
		}
		coverage--
	}
	if coverage < 2 {
		return 0
	}
	for coverage < len(messages) && messages[coverage].Role == "tool" {
		coverage++
	}
	if coverage > 0 && messages[coverage-1].Role == "assistant" && len(messages[coverage-1].ToolCalls) > 0 {
		coverage--
	}
	return coverage
}

func chooseContextCompactionChunk(messages []store.Message, start, maxCoverage int, previous *store.ContextCheckpoint, maxRequestTokens int) int {
	if start >= maxCoverage || maxRequestTokens <= 0 {
		return start
	}

	best := start
	for candidate := start + 1; candidate <= maxCoverage; candidate++ {
		if wouldCutToolBoundary(messages, candidate) {
			continue
		}
		if estimateContextCheckpointRequestTokens(previous, messages[start:candidate], start) > maxRequestTokens {
			if best == start {
				return candidate
			}
			break
		}
		best = candidate
	}
	return best
}

func wouldCutToolBoundary(messages []store.Message, coverage int) bool {
	if coverage <= 0 || coverage >= len(messages) {
		return false
	}
	if messages[coverage].Role == "tool" {
		return true
	}
	return messages[coverage-1].Role == "assistant" && len(messages[coverage-1].ToolCalls) > 0
}

func estimateContextCheckpointRequestTokens(previous *store.ContextCheckpoint, messages []store.Message, messageOffset int) int {
	return estimateTokens(contextCheckpointPrompt(previous, messages, messageOffset, 0)) + 128
}

func (s *Server) generateContextCheckpoint(ctx context.Context, c chatClient, model string, previous *store.ContextCheckpoint, messages []store.Message, messageOffset int, maxRequestTokens int) (string, error) {
	maxPromptChars := 0
	if maxRequestTokens > 128 {
		maxPromptChars = (maxRequestTokens - 128) * 4
	}
	rendered := contextCheckpointPrompt(previous, messages, messageOffset, maxPromptChars)
	outputBudget := max(256, min(contextCompactionMaxOutput, maxRequestTokens/4))

	req := &api.ChatRequest{
		Model: model,
		Messages: []api.Message{
			{
				Role:    "system",
				Content: "Create a compact semantic checkpoint of prior conversation state for continuing technical work. Preserve user requests, useful assistant answers, active objective, current state, confirmed decisions, discoveries, important paths/symbols, changes made, tests/results, failed approaches, constraints, unresolved questions, and immediate next steps. Tool payloads are intentionally omitted; preserve only conclusions already stated by the assistant. Do not write a transcript. Do not invent facts.",
			},
			{
				Role:    "user",
				Content: rendered,
			},
		},
		Stream:  ptr(false),
		Options: map[string]any{"num_predict": outputBudget},
	}

	var out strings.Builder
	err := c.Chat(ctx, req, func(res api.ChatResponse) error {
		out.WriteString(res.Message.Content)
		return nil
	})
	if err != nil {
		slog.Debug("context compaction internal chat failed", "error", err)
		return "", err
	}
	return out.String(), nil
}

func contextCheckpointPrompt(previous *store.ContextCheckpoint, messages []store.Message, messageOffset int, maxChars int) string {
	var rendered strings.Builder
	var remaining *int
	if maxChars > 0 {
		budget := maxChars
		remaining = &budget
	}
	if previous != nil {
		rendered.WriteString("Existing valid checkpoint covering canonical messages [0:")
		rendered.WriteString(fmt.Sprint(previous.CoveredMessageCount))
		rendered.WriteString("]. Consolidate it with only the canonical delta below. The new result must represent the full covered prefix after the delta, not just the delta.\n\n")
		writeCompactionField(&rendered, previous.Content, remaining)
		rendered.WriteString("\n\nCanonical delta newly covered:\n")
	}
	for i, msg := range messages {
		if msg.Role == "tool" {
			continue
		}
		rendered.WriteString(fmt.Sprintf("\n[%d] %s:\n", messageOffset+i+1, msg.Role))
		writeCompactionField(&rendered, msg.Content, remaining)
		rendered.WriteString("\n")
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				rendered.WriteString("TOOL CALLED: " + tc.Function.Name + "\n")
			}
		}
		if len(msg.ToolCalls) == 0 && msg.ToolName != "" {
			rendered.WriteString("TOOL CALLED: " + msg.ToolName + "\n")
		}
		if len(msg.Attachments) > 0 {
			rendered.WriteString(fmt.Sprintf("attachments: %d\n", len(msg.Attachments)))
			for _, att := range msg.Attachments {
				rendered.WriteString("- " + att.Filename + "\n")
			}
		}
	}
	return rendered.String()
}

func writeCompactionField(rendered *strings.Builder, value string, remaining *int) {
	if value == "" {
		return
	}
	if remaining == nil {
		rendered.WriteString(value)
		return
	}
	if *remaining <= 0 {
		rendered.WriteString(fmt.Sprintf("[... omitted %d chars from oversized canonical field for checkpoint generation ...]", len(value)))
		return
	}
	if len(value) <= *remaining {
		rendered.WriteString(value)
		*remaining -= len(value)
		return
	}

	maxChars := *remaining
	*remaining = 0
	if maxChars < 256 {
		rendered.WriteString(fmt.Sprintf("[... omitted %d chars from oversized canonical field for checkpoint generation ...]", len(value)))
		return
	}

	omission := fmt.Sprintf("\n[... omitted %d chars from oversized canonical field for checkpoint generation ...]\n", len(value)-maxChars)
	bodyChars := maxChars - len(omission)
	if bodyChars < 128 {
		rendered.WriteString(fmt.Sprintf("[... omitted %d chars from oversized canonical field for checkpoint generation ...]", len(value)))
		return
	}
	head := bodyChars / 2
	tail := bodyChars - head
	rendered.WriteString(value[:head])
	rendered.WriteString(omission)
	rendered.WriteString(value[len(value)-tail:])
}

func estimateMessagesTokens(messages []store.Message) int {
	total := 0
	for _, msg := range messages {
		total += estimateMessageTokens(msg)
	}
	return total
}

func estimateMessageTokens(msg store.Message) int {
	total := estimateTokens(msg.Role) + estimateTokens(msg.Content) + estimateTokens(msg.Thinking) + 8
	for _, att := range msg.Attachments {
		total += estimateTokens(att.Filename)
		if isImageAttachment(att.Filename) {
			total += api.ImageTokenEstimate
		} else {
			total += len(att.Data) / 4
		}
	}
	for _, tc := range msg.ToolCalls {
		total += estimateTokens(tc.Function.Name) + estimateTokens(tc.Function.Arguments) + 16
	}
	if msg.ToolName != "" {
		total += estimateTokens(msg.ToolName)
	}
	return total
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}
