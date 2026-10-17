import { describe, expect, it } from "vitest";
import { recommendDefaultModel } from "./useSelectedModel";

describe("recommendDefaultModel", () => {
  it("keeps the current VRAM-only recommendation when fallback is disabled", () => {
    expect(recommendDefaultModel(4, false, 64)).toBe("gemma3:1b");
    expect(recommendDefaultModel(8, false, 64)).toBe("gemma3:4b");
    expect(recommendDefaultModel(24, false, 64)).toBe("gpt-oss:20b");
  });

  it("promotes small-VRAM systems to a CPU-friendly local default when fallback is enabled", () => {
    expect(recommendDefaultModel(4, true, 24)).toBe("gemma3:4b");
    expect(recommendDefaultModel(4, true, 32)).toBe("gemma3:4b");
  });

  it("does not promote when the CPU memory budget is still too small", () => {
    expect(recommendDefaultModel(4, true, 12)).toBe("gemma3:1b");
  });
});
