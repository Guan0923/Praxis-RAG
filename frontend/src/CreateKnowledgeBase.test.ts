import { describe, expect, it } from "vitest";
import { matchingProfile, profileNeedsReembed } from "./CreateKnowledgeBase";
import type { KnowledgeBase, ModelProfile } from "./api";

function profile(overrides: Partial<ModelProfile> = {}): ModelProfile {
  return {
    id: "mp-embed",
    kind: "embedding",
    name: "默认 Embedding",
    base_url: "https://api.example.com/v1",
    model: "bge-m3",
    api_key_set: true,
    vector_size: 1024,
    ...overrides,
  };
}

// Only the fields the matching / reembed helpers read are needed.
const initial = {
  embedding: { base_url: "https://api.example.com/v1", api_key_set: true },
  embedding_model: "bge-m3",
} as KnowledgeBase;

describe("matchingProfile", () => {
  it("finds the profile with the same kind, model and base URL", () => {
    expect(matchingProfile([profile()], "embedding", "https://api.example.com/v1", "bge-m3"))
      .toMatchObject({ id: "mp-embed" });
  });

  it("ignores a trailing slash in the stored base URL", () => {
    expect(matchingProfile([profile({ base_url: "https://api.example.com/v1/" })],
      "embedding", "https://api.example.com/v1", "bge-m3")).toMatchObject({ id: "mp-embed" });
    expect(matchingProfile([profile()], "embedding", "https://api.example.com/v1/", "bge-m3"))
      .toMatchObject({ id: "mp-embed" });
  });

  it("never matches a different kind, model or host", () => {
    expect(matchingProfile([profile({ kind: "reranker" })], "embedding",
      "https://api.example.com/v1", "bge-m3")).toBeUndefined();
    expect(matchingProfile([profile({ model: "bge-large" })], "embedding",
      "https://api.example.com/v1", "bge-m3")).toBeUndefined();
    expect(matchingProfile([profile()], "embedding",
      "https://other.example.com/v1", "bge-m3")).toBeUndefined();
  });
});

describe("profileNeedsReembed", () => {
  it("does not require a reembed without an edit target or selection", () => {
    expect(profileNeedsReembed(undefined, profile())).toBe(false);
    expect(profileNeedsReembed(initial, undefined)).toBe(false);
  });

  it("keeps the current connection when the selected profile matches it", () => {
    expect(profileNeedsReembed(initial, profile())).toBe(false);
    expect(profileNeedsReembed(initial, profile({ base_url: "https://api.example.com/v1/" })))
      .toBe(false);
  });

  it("requires a reembed for a different embedding connection or model", () => {
    expect(profileNeedsReembed(initial, profile({ base_url: "https://other.example.com/v1" })))
      .toBe(true);
    expect(profileNeedsReembed(initial, profile({ model: "bge-large" }))).toBe(true);
  });

  it("ignores non-embedding profiles", () => {
    expect(profileNeedsReembed(initial, profile({ kind: "reranker" }))).toBe(false);
  });
});
