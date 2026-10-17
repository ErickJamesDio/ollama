//go:build windows || darwin

package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
)

type fakeCompactionClient struct {
	content  string
	contents []string
	err      error
	calls    int
	requests []*api.ChatRequest
}

func (f *fakeCompactionClient) Chat(_ context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	f.calls++
	f.requests = append(f.requests, req)
	if f.err != nil {
		return f.err
	}
	content := f.content
	if len(f.contents) >= f.calls {
		content = f.contents[f.calls-1]
	}
	return fn(api.ChatResponse{Message: api.Message{Content: content}})
}

func testCompactionServer(t *testing.T, contextLength int) *Server {
	t.Helper()
	st := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	settings, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.ContextLength = contextLength
	if err := st.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	return &Server{Store: st, Logger: slogDiscard()}
}

func persistChat(t *testing.T, server *Server, chat *store.Chat) {
	t.Helper()
	if err := server.Store.SetChat(*chat); err != nil {
		t.Fatal(err)
	}
}

func manyMessages(count int, content string) []store.Message {
	msgs := make([]store.Message, 0, count)
	for i := 0; i < count; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, store.NewMessage(role, content, nil))
	}
	return msgs
}

func TestContextCompactionBelowThresholdUnchanged(t *testing.T) {
	server := testCompactionServer(t, 4096)
	client := &fakeCompactionClient{content: "checkpoint"}
	chat := &store.Chat{ID: "chat-1", Messages: manyMessages(2, "short")}

	got, err := server.operationalChatForRequest(context.Background(), client, chat, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	if got != chat {
		t.Fatal("expected below-threshold chat to remain unchanged")
	}
	if client.calls != 0 {
		t.Fatalf("compaction client calls = %d, want 0", client.calls)
	}
}

func TestContextCompactionCheckpointAndTailProjection(t *testing.T) {
	server := testCompactionServer(t, 400)
	client := &fakeCompactionClient{content: "active objective and decisions"}
	chat := &store.Chat{ID: "chat-1", Messages: manyMessages(12, strings.Repeat("history ", 20))}
	originalLen := len(chat.Messages)
	persistChat(t, server, chat)

	got, err := server.operationalChatForRequest(context.Background(), client, chat, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	if got == chat {
		t.Fatal("expected compacted projection")
	}
	if len(chat.Messages) != originalLen {
		t.Fatal("canonical chat messages were mutated")
	}
	if got.Messages[0].Role != "system" || !strings.Contains(got.Messages[0].Content, "Compacted prior conversation state") {
		t.Fatalf("first projected message = %+v, want checkpoint system message", got.Messages[0])
	}
	checkpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == nil || !strings.Contains(checkpoint.Content, "active objective") {
		t.Fatalf("checkpoint = %+v, want saved semantic content", checkpoint)
	}
}

func TestContextCheckpointInvalidatesAfterHistoricalEdit(t *testing.T) {
	server := testCompactionServer(t, 200)
	chat := store.NewChat("chat-1")
	chat.Messages = manyMessages(8, strings.Repeat("history ", 20))
	if err := server.Store.SetChat(*chat); err != nil {
		t.Fatal(err)
	}
	cp := store.ContextCheckpoint{
		ChatID:                chat.ID,
		CoveredMessageCount:   4,
		CoveredFingerprint:    store.MessagePrefixFingerprint(chat.Messages, 4),
		Content:               "checkpoint",
		EstimatedTokensBefore: 100,
		EstimatedTokensAfter:  40,
	}
	if err := server.Store.SaveContextCheckpoint(cp); err != nil {
		t.Fatal(err)
	}

	chat.Messages[1].Content = "edited historical message"
	if err := server.Store.SetChat(*chat); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != nil {
		t.Fatal("expected checkpoint invalidated after covered history edit")
	}
}

func TestContextCompactionRepeatedSupersedesCheckpoint(t *testing.T) {
	server := testCompactionServer(t, 800)
	chat := &store.Chat{ID: "chat-1", Messages: manyMessages(12, strings.Repeat("history ", 20))}
	chat.Messages[0].Content = "old-prefix-should-not-be-sent-again " + chat.Messages[0].Content
	persistChat(t, server, chat)

	first := &fakeCompactionClient{content: "first checkpoint"}
	if _, err := server.operationalChatForRequest(context.Background(), first, chat, "test-model"); err != nil {
		t.Fatal(err)
	}
	second := &fakeCompactionClient{content: "second checkpoint"}
	chat.Messages = append(chat.Messages, manyMessages(6, "new-delta-only "+strings.Repeat("more work ", 20))...)
	if _, err := server.operationalChatForRequest(context.Background(), second, chat, "test-model"); err != nil {
		t.Fatal(err)
	}
	if len(second.requests) != 1 {
		t.Fatalf("second compaction requests = %d, want 1", len(second.requests))
	}
	if len(second.requests[0].Messages) < 2 {
		t.Fatalf("second compaction messages = %+v, want system + user", second.requests[0].Messages)
	}
	secondInput := second.requests[0].Messages[1].Content
	if !strings.Contains(secondInput, "first checkpoint") {
		t.Fatal("second compaction input did not include previous checkpoint")
	}
	if !strings.Contains(secondInput, "new-delta-only") {
		t.Fatal("second compaction input did not include newly covered canonical delta")
	}
	if strings.Contains(secondInput, "old-prefix-should-not-be-sent-again") {
		t.Fatal("second compaction input included the full old canonical prefix")
	}

	checkpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == nil || !strings.Contains(checkpoint.Content, "second checkpoint") {
		t.Fatalf("checkpoint = %+v, want latest checkpoint", checkpoint)
	}
}

func TestContextCompactionChunksLargeDelta(t *testing.T) {
	server := testCompactionServer(t, 700)
	chat := &store.Chat{ID: "chat-1", Messages: manyMessages(12, strings.Repeat("history ", 20))}
	persistChat(t, server, chat)

	first := &fakeCompactionClient{content: "first checkpoint"}
	if _, err := server.operationalChatForRequest(context.Background(), first, chat, "test-model"); err != nil {
		t.Fatal(err)
	}
	firstCheckpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if firstCheckpoint == nil {
		t.Fatal("expected first compaction to save a checkpoint")
	}

	second := &fakeCompactionClient{
		content: "second checkpoint final",
		contents: []string{
			"second checkpoint part 1",
			"second checkpoint part 2",
			"second checkpoint part 3",
		},
	}
	chat.Messages = append(chat.Messages, manyMessages(8, "chunk-delta "+strings.Repeat("tool output ", 80))...)
	if _, err := server.operationalChatForRequest(context.Background(), second, chat, "test-model"); err != nil {
		t.Fatal(err)
	}

	if second.calls < 2 {
		t.Fatalf("second compaction calls = %d, want chunked incremental compaction", second.calls)
	}
	maxRequestTokens := int(float64(700) * contextCompactionChunkRatio)
	for i, req := range second.requests {
		if len(req.Messages) < 2 {
			t.Fatalf("request %d messages = %+v, want system + user", i, req.Messages)
		}
		got := estimateTokens(req.Messages[1].Content) + 128
		if got > maxRequestTokens {
			t.Fatalf("request %d estimated tokens = %d, want <= %d", i, got, maxRequestTokens)
		}
	}
	if strings.Contains(second.requests[0].Messages[1].Content, "second checkpoint part 1") {
		t.Fatal("first chunk should start from the previous persisted checkpoint, not its own output")
	}
	if !strings.Contains(second.requests[1].Messages[1].Content, "second checkpoint part 1") {
		t.Fatal("second chunk did not consolidate the checkpoint produced by the first chunk")
	}

	checkpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == nil {
		t.Fatal("expected checkpoint after chunked compaction")
	}
	if checkpoint.CoveredMessageCount <= firstCheckpoint.CoveredMessageCount {
		t.Fatalf("checkpoint covered = %d, want > first covered %d", checkpoint.CoveredMessageCount, firstCheckpoint.CoveredMessageCount)
	}
}

func TestContextCompactionTruncatesOversizedSingleDeltaMessage(t *testing.T) {
	server := testCompactionServer(t, 1200)
	chat := &store.Chat{ID: "chat-1", Messages: manyMessages(12, strings.Repeat("history ", 80))}
	persistChat(t, server, chat)

	first := &fakeCompactionClient{content: "first checkpoint"}
	if _, err := server.operationalChatForRequest(context.Background(), first, chat, "test-model"); err != nil {
		t.Fatal(err)
	}
	firstCheckpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if firstCheckpoint == nil {
		t.Fatal("expected first compaction to save a checkpoint")
	}

	hugeToolResult := strings.Repeat("oversized-tool-result ", 4000)
	toolMessage := store.NewMessage("tool", hugeToolResult, nil)
	toolMessage.ToolName = "big_tool"
	chat.Messages = append(chat.Messages,
		store.NewMessage("assistant", "", &store.MessageOptions{ToolCalls: []store.ToolCall{{Type: "function", Function: store.ToolFunction{Name: "big_tool", Arguments: "{}"}}}}),
		toolMessage,
		store.NewMessage("user", "continue after huge tool output", nil),
	)

	second := &fakeCompactionClient{content: "checkpoint after huge tool output"}
	if _, err := server.operationalChatForRequest(context.Background(), second, chat, "test-model"); err != nil {
		t.Fatal(err)
	}
	if second.calls == 0 {
		t.Fatal("expected compaction call for huge delta")
	}
	for _, req := range second.requests {
		input := req.Messages[1].Content
		if strings.Contains(input, hugeToolResult) || strings.Contains(input, "oversized-tool-result") {
			t.Fatal("compaction request included raw tool result")
		}
	}

	checkpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == nil || checkpoint.CoveredMessageCount <= firstCheckpoint.CoveredMessageCount {
		t.Fatalf("checkpoint = %+v, want advanced coverage after oversized delta", checkpoint)
	}
}

func TestContextCheckpointPromptExcludesThinkingAndToolArguments(t *testing.T) {
	toolResult := "raw tool payload must not enter checkpoint"
	messages := []store.Message{
		store.NewMessage("user", "inspect the workspace", nil),
		store.NewMessage("assistant", "calling tool", &store.MessageOptions{
			Thinking: "private reasoning must not enter checkpoint",
			ToolCalls: []store.ToolCall{{
				Type: "function",
				Function: store.ToolFunction{
					Name:      "inspect_workspace",
					Arguments: `{"secret":"full arguments must not enter checkpoint"}`,
				},
			}},
		}),
		store.NewMessage("tool", toolResult, nil),
		store.NewMessage("assistant", "The relevant conclusion is that the build passed.", nil),
	}

	got := contextCheckpointPrompt(nil, messages, 0, 0)
	if strings.Contains(got, "private reasoning") {
		t.Fatal("checkpoint prompt included assistant thinking")
	}
	if strings.Contains(got, "full arguments") {
		t.Fatal("checkpoint prompt included tool-call arguments")
	}
	if !strings.Contains(got, "TOOL CALLED: inspect_workspace") {
		t.Fatal("checkpoint prompt did not preserve normalized tool marker")
	}
	if strings.Contains(got, toolResult) || strings.Contains(got, "raw tool payload") {
		t.Fatal("checkpoint prompt included raw tool result")
	}
	if !strings.Contains(got, "inspect the workspace") || !strings.Contains(got, "The relevant conclusion is that the build passed.") {
		t.Fatalf("checkpoint prompt did not preserve user request and useful assistant conclusion: %q", got)
	}
}

func TestContextProjectionRecoversTwoMegabyteToolResultAfterCheckpoint(t *testing.T) {
	const contextLength = 32768
	server := testCompactionServer(t, contextLength)
	chat := &store.Chat{ID: "chat-large-tool", Messages: manyMessages(13, "preserved history")}
	persistChat(t, server, chat)
	checkpoint := store.ContextCheckpoint{
		ChatID:              chat.ID,
		CoveredMessageCount: 13,
		CoveredFingerprint:  store.MessagePrefixFingerprint(chat.Messages, 13),
		Content:             "Known state before the tool call.",
	}
	if err := server.Store.SaveContextCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}

	toolCall := store.NewMessage("assistant", "", &store.MessageOptions{
		Thinking: "private reasoning must not enter recovery projection",
		ToolCalls: []store.ToolCall{{
			Type: "function",
			Function: store.ToolFunction{
				Name:      "mcp.filesystem.read_file",
				Arguments: `{"path":"large.txt","operational":"metadata"}`,
			},
		}},
	})
	hugeResult := strings.Repeat("0123456789abcdef", 128*1024) // 2 MiB
	toolResult := store.NewMessage("tool", hugeResult, nil)
	toolResult.ToolName = "mcp.filesystem.read_file"
	chat.Messages = append(chat.Messages, toolCall, toolResult)
	persistChat(t, server, chat)

	registry := tools.NewRegistry()
	registerToolResultRecovery(registry, chat, contextLength)
	definitions := registry.AvailableTools()
	if _, ok := registry.Get(contextRecoveryToolName); !ok {
		t.Fatal("oversized canonical tool result did not register recovery tool")
	}

	canonicalResult := chat.Messages[len(chat.Messages)-1].Content
	projected, err := server.operationalChatForRequestWithTraceAndOverhead(
		context.Background(), &fakeCompactionClient{content: "unused"}, chat, "test-model", estimateToolDefinitionsTokens(definitions), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Messages[len(chat.Messages)-1].Content != canonicalResult || len(canonicalResult) != 2*1024*1024 {
		t.Fatal("canonical SQLite message was modified")
	}
	if len(projected.Messages) != 3 {
		t.Fatalf("projected messages = %d, want checkpoint + assistant + tool", len(projected.Messages))
	}
	if projected.Messages[1].Thinking != "" {
		t.Fatal("thinking leaked into recovery projection")
	}
	if got := projected.Messages[1].ToolCalls[0].Function.Arguments; got != "{}" {
		t.Fatalf("tool arguments = %q, want normalized empty object", got)
	}
	projectedResult := projected.Messages[2].Content
	if len(projectedResult) >= len(canonicalResult) || !strings.Contains(projectedResult, "result_ref=canonical-tool-result:14") {
		t.Fatalf("oversized result was not replaced by recoverable partial view: %d bytes", len(projectedResult))
	}
	if !strings.Contains(projectedResult, "Do not claim the complete result was analyzed") {
		t.Fatal("partial view does not disclose incomplete analysis")
	}

	req, err := server.buildChatRequest(projected, "test-model", nil, definitions)
	if err != nil {
		t.Fatal(err)
	}
	if got, budget := estimateAPIChatRequestTokens(req), contextFinalRequestBudget(contextLength); got > budget {
		t.Fatalf("final request estimate = %d, want <= %d", got, budget)
	}

	_, chunk, err := registry.Execute(context.Background(), contextRecoveryToolName, map[string]any{
		"result_ref": "canonical-tool-result:14",
		"offset":     float64(contextRecoveryPreviewBytes),
		"max_chars":  float64(8192),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chunk, "complete=false") || !strings.Contains(chunk, "next_offset=12288") {
		t.Fatalf("unexpected recovery chunk metadata: %q", chunk[:min(len(chunk), 256)])
	}
}

func TestContextCheckpointGenerationHasOutputBudget(t *testing.T) {
	server := testCompactionServer(t, 4096)
	client := &fakeCompactionClient{content: "checkpoint"}
	_, err := server.generateContextCheckpoint(context.Background(), client, "test-model", nil, manyMessages(2, "history"), 0, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	if got := client.requests[0].Options["num_predict"]; got != 750 {
		t.Fatalf("num_predict = %v, want 750", got)
	}
}

func TestContextCompactionAvoidsStartingAtToolMessage(t *testing.T) {
	messages := []store.Message{
		store.NewMessage("user", strings.Repeat("old ", 40), nil),
		store.NewMessage("assistant", "", &store.MessageOptions{ToolCalls: []store.ToolCall{{Type: "function", Function: store.ToolFunction{Name: "tool", Arguments: "{}"}}}}),
		store.NewMessage("tool", "tool result", nil),
		store.NewMessage("user", strings.Repeat("recent ", 40), nil),
		store.NewMessage("assistant", "answer", nil),
	}
	coverage := chooseContextCoverage(messages, 10)
	if coverage < len(messages) && messages[coverage].Role == "tool" {
		t.Fatalf("coverage starts tail at tool message: %d", coverage)
	}
	if coverage > 0 && messages[coverage-1].Role == "assistant" && len(messages[coverage-1].ToolCalls) > 0 {
		t.Fatalf("coverage cuts immediately after assistant tool call: %d", coverage)
	}
}

func TestContextCompactionGenerationFailureKeepsCanonical(t *testing.T) {
	server := testCompactionServer(t, 800)
	client := &fakeCompactionClient{err: context.Canceled}
	chat := &store.Chat{ID: "chat-1", Messages: manyMessages(12, strings.Repeat("history ", 20))}
	persistChat(t, server, chat)

	got, err := server.operationalChatForRequest(context.Background(), client, chat, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	if got != chat {
		t.Fatal("expected canonical chat after checkpoint generation failure")
	}
	checkpoint, err := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != nil {
		t.Fatal("failed compaction should not save checkpoint")
	}
}

func TestContextCompactionGenerationFailureOverLimitFailsSafely(t *testing.T) {
	server := testCompactionServer(t, 200)
	client := &fakeCompactionClient{err: context.Canceled}
	chat := &store.Chat{ID: "chat-1", Messages: manyMessages(12, strings.Repeat("history ", 20))}
	persistChat(t, server, chat)

	got, err := server.operationalChatForRequest(context.Background(), client, chat, "test-model")

	if err == nil {
		t.Fatal("expected explicit error when canonical context already exceeds limit")
	}
	if got != nil {
		t.Fatal("expected no oversized canonical fallback")
	}
	checkpoint, checkpointErr := server.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if checkpointErr != nil {
		t.Fatal(checkpointErr)
	}
	if checkpoint != nil {
		t.Fatal("failed over-limit compaction should not save checkpoint")
	}
}

func TestEstimateImageAttachmentTokensUsesCanonicalAttachmentCost(t *testing.T) {
	messages := []store.Message{
		{
			Role: "user",
			Attachments: []store.File{
				{Filename: "scan.png", Data: make([]byte, 400)},
				{Filename: "notes.txt", Data: make([]byte, 800)},
			},
		},
		{
			Role: "user",
			Attachments: []store.File{
				{Filename: "photo.jpg", Data: make([]byte, 200)},
			},
		},
	}

	tokens, bytes, count := estimateImageAttachmentTokens(messages)
	wantTokens := estimateTokens("scan.png") + api.ImageTokenEstimate + estimateTokens("photo.jpg") + api.ImageTokenEstimate
	if tokens != wantTokens {
		t.Fatalf("image tokens = %d, want %d", tokens, wantTokens)
	}
	if bytes != 600 {
		t.Fatalf("image bytes = %d, want 600", bytes)
	}
	if count != 2 {
		t.Fatalf("image count = %d, want 2", count)
	}

	imageMessageTokens := estimateMessageTokens(messages[0])
	wantMessageTokens := estimateTokens("user") + 8 + estimateTokens("scan.png") + api.ImageTokenEstimate + estimateTokens("notes.txt") + 800/4
	if imageMessageTokens != wantMessageTokens {
		t.Fatalf("message tokens = %d, want %d", imageMessageTokens, wantMessageTokens)
	}
}

func TestCompactionProgressUsesVisualTokenCost(t *testing.T) {
	server := testCompactionServer(t, 131072)
	chat := &store.Chat{
		ID: "chat-image",
		Messages: []store.Message{{
			Role: "user",
			Attachments: []store.File{{
				Filename: "large.png",
				Data:     make([]byte, 958067),
			}},
		}},
	}
	persistChat(t, server, chat)

	if shouldShowContextCompactionProgress(server, chat) {
		t.Fatal("compressed image bytes must not trigger context compaction")
	}
}

func TestCompactionProgressIgnoresCanonicalHistoryCoveredByCheckpoint(t *testing.T) {
	server := testCompactionServer(t, 800)
	chat := &store.Chat{ID: "chat-checkpoint", Messages: manyMessages(12, strings.Repeat("history ", 20))}
	persistChat(t, server, chat)

	checkpoint := store.ContextCheckpoint{
		ChatID:              chat.ID,
		CoveredMessageCount: 10,
		CoveredFingerprint:  store.MessagePrefixFingerprint(chat.Messages, 10),
		Content:             "compact state",
	}
	if err := server.Store.SaveContextCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}

	if shouldShowContextCompactionProgress(server, chat) {
		t.Fatal("covered canonical history must not display compaction progress again")
	}
}
