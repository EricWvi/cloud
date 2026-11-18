import { describe, expect, it } from "vitest";
import {
  workflowAgentCompletionIssues,
  workflowAgentCompletionPathIssue,
} from "./agent-completion";
import { agentConfigNeedsSchemaVersion4 } from "./validation";

// The editor's path rules mirror v2-server's (internal/workflow/completion.go), which follows
// desktop's RelativeGlob: a path the editor accepts must not be refused at publication.
describe("completion file declarations", () => {
  it.each([
    ["", "empty"],
    ["/abs.md", "absolute"],
    ["C:/x.md", "absolute"],
    ["a\\b.md", "backslash"],
    ["!a.md", "negation"],
    ["a/[x].md", "unsupportedGlobCharacter"],
    ["a//b.md", "emptySegment"],
    ["a/../b.md", "parentTraversal"],
    ["dir/d:x.md", "drivePrefix"],
    ["con.txt", "reservedName"],
    ["a**/b.md", "invalidGlobstar"],
  ])("refuses %j as %s", (path, issue) => {
    expect(workflowAgentCompletionPathIssue(path)).toBe(issue);
  });

  it.each(["plan.md", "reports/**/*.md", "a?.txt", "console.txt", "notes:v2.md"])(
    "accepts %j",
    (path) => {
      expect(workflowAgentCompletionPathIssue(path)).toBeNull();
    },
  );

  it("bounds the declaration and rejects unknown fields", () => {
    expect(workflowAgentCompletionIssues(undefined)).toEqual([]);
    expect(workflowAgentCompletionIssues({ files: [] })).toHaveLength(1);
    expect(
      workflowAgentCompletionIssues({
        files: Array.from({ length: 33 }, (_, i) => ({ path: `f${i}.md` })),
      }),
    ).toHaveLength(1);
    expect(workflowAgentCompletionIssues({ files: [{ path: "a.md" }], criteria: "x" })).toHaveLength(1);
    expect(workflowAgentCompletionIssues({ files: [{ path: "a.md", mustChange: "yes" }] })).toHaveLength(1);
  });

  it("needs version 4 for any completion field", () => {
    expect(agentConfigNeedsSchemaVersion4({})).toBe(false);
    expect(agentConfigNeedsSchemaVersion4({ completionSignal: "end_turn" })).toBe(true);
    expect(agentConfigNeedsSchemaVersion4({ autoAcceptAgentCompletion: false })).toBe(true);
    expect(agentConfigNeedsSchemaVersion4({ completion: { files: [{ path: "a.md" }] } })).toBe(true);
  });
});
