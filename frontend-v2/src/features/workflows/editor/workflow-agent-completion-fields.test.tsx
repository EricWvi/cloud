import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import type { Node } from "@xyflow/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMockWorkflowCapabilities,
  type WorkflowAgentConfig,
  type WorkflowNodeData,
} from "@/lib/workflows/mock";
import { appI18n } from "@/lib/workflows/test/i18n-instance";
import { AppI18nProvider } from "@/lib/workflows/test/i18n";
import { WorkflowInspector } from "./workflow-inspector";

/** Builds an interactive Agent node whose configuration is edited by the inspector. */
function agentNode(
  overrides: Partial<WorkflowAgentConfig> = {},
): Node<WorkflowNodeData, "workflow"> {
  return {
    id: "agent-1",
    type: "workflow",
    position: { x: 0, y: 0 },
    data: {
      kind: "agent",
      title: "Review",
      description: "",
      agentConfig: {
        ...createMockWorkflowCapabilities("en-US").defaultAgentConfig,
        interactive: true,
        ...overrides,
      } as WorkflowAgentConfig,
    },
  };
}

/** Applies inspector edits to local state and exposes the persisted Agent contract. */
function Harness({
  node,
  unappliedSettings = [],
}: {
  node: Node<WorkflowNodeData, "workflow">;
  unappliedSettings?: string[];
}) {
  const [current, setCurrent] = useState(node);
  return (
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <AppI18nProvider>
        <WorkflowInspector
          variableCatalog={[]}
          node={current}
          capabilities={createMockWorkflowCapabilities("en-US")}
          unappliedSettings={unappliedSettings}
          onUpdate={setCurrent}
          onDelete={() => undefined}
          onCloseNode={() => undefined}
        />
        <pre data-testid="agent-config">
          {JSON.stringify(current.data.agentConfig)}
        </pre>
      </AppI18nProvider>
    </QueryClientProvider>
  );
}

/** Reads the Agent contract exactly as it would be serialized into the graph. */
function persistedConfig(): Record<string, unknown> {
  return JSON.parse(screen.getByTestId("agent-config").textContent ?? "");
}

describe("Agent completion files", () => {
  it("adds paths and writes version 4 with the first declaration", async () => {
    await appI18n.changeLanguage("en-US");
    const user = userEvent.setup();
    render(<Harness node={agentNode()} />);

    expect(persistedConfig()).not.toHaveProperty("completion");
    expect(persistedConfig().schemaVersion).toBe(3);
    expect(
      screen.getByText("No files declared; completion does no file check."),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Add file" }));
    expect(persistedConfig()).toMatchObject({
      schemaVersion: 4,
      completion: { files: [{ path: "" }] },
    });

    await user.type(screen.getByLabelText("File 1 path"), "reports/*.md");
    await user.click(screen.getByRole("switch", { name: /must change/iu }));

    expect(persistedConfig()).toMatchObject({
      schemaVersion: 4,
      completion: {
        files: [{ path: "reports/*.md", mustChange: true }],
      },
    });
  });

  it("shows a localized path error inline and clears it after keyboard correction", async () => {
    await appI18n.changeLanguage("en-US");
    const user = userEvent.setup();
    render(<Harness node={agentNode()} />);

    await user.click(screen.getByRole("button", { name: "Add file" }));
    const input = screen.getByLabelText("File 1 path");
    await user.click(input);
    expect(input).toHaveFocus();
    await user.type(input, "../outside.md");

    expect(input).toHaveAttribute("aria-invalid", "true");
    const error = screen.getByRole("alert");
    expect(error).toHaveTextContent("The path must not contain a .. segment.");
    expect(input).toHaveAttribute(
      "aria-describedby",
      expect.stringContaining(error.id),
    );

    await user.clear(input);
    await user.type(input, "reports/*.md");
    await user.tab();
    expect(input).toHaveAttribute("aria-invalid", "false");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("keeps declaration order and removes files without losing the others", async () => {
    await appI18n.changeLanguage("en-US");
    const user = userEvent.setup();
    render(<Harness node={agentNode()} />);

    await user.click(screen.getByRole("button", { name: "Add file" }));
    await user.type(screen.getByLabelText("File 1 path"), "reports/*.md");
    await user.click(screen.getByRole("button", { name: "Add file" }));
    await user.type(screen.getByLabelText("File 2 path"), "reports/final.md");

    expect(persistedConfig()).toMatchObject({
      completion: {
        files: [{ path: "reports/*.md" }, { path: "reports/final.md" }],
      },
    });

    await user.click(screen.getByRole("button", { name: "Remove file 1" }));
    expect(persistedConfig()).toMatchObject({
      completion: { files: [{ path: "reports/final.md" }] },
    });
    expect(screen.getByLabelText("File 1 path")).toHaveValue(
      "reports/final.md",
    );

    await user.click(screen.getByRole("button", { name: "Remove file 1" }));
    expect(persistedConfig()).not.toHaveProperty("completion");
    expect(persistedConfig().schemaVersion).toBe(3);
  });

  it("keeps version 4 when the last file is removed from a signal node", async () => {
    await appI18n.changeLanguage("en-US");
    const user = userEvent.setup();
    render(
      <Harness
        node={agentNode({ schemaVersion: 4, completionSignal: "agent" })}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Add file" }));
    await user.type(screen.getByLabelText("File 1 path"), "reports/*.md");
    await user.click(screen.getByRole("button", { name: "Remove file 1" }));

    expect(persistedConfig()).not.toHaveProperty("completion");
    expect(persistedConfig()).toMatchObject({
      schemaVersion: 4,
      completionSignal: "agent",
    });
  });

  it("offers auto-accept only for the agent policy and writes it with version 4", async () => {
    await appI18n.changeLanguage("en-US");
    const user = userEvent.setup();
    const { unmount } = render(<Harness node={agentNode()} />);
    // v2 defaults to end_turn, where auto-accept does not exist.
    expect(
      document.getElementById("workflow-agent-auto-accept-completion"),
    ).toBeNull();
    unmount();
    render(
      <Harness
        node={agentNode({ schemaVersion: 4, completionSignal: "agent" })}
      />,
    );

    const toggle = document.getElementById(
      "workflow-agent-auto-accept-completion",
    );
    expect(toggle).not.toBeNull();
    await user.click(toggle as HTMLElement);

    expect(persistedConfig()).toMatchObject({
      schemaVersion: 4,
      completionSignal: "agent",
      autoAcceptAgentCompletion: true,
    });
  });

  it("explains that files are checked on completion and shows declared count", async () => {
    await appI18n.changeLanguage("en-US");
    render(<Harness node={agentNode()} />);

    expect(
      screen.getByText(
        /Files are checked when you click complete: a missing file or an unchanged `mustChange` file blocks completion/,
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("0 / 32 files")).toBeInTheDocument();
  });
  it("keeps a malformed file row visible and lets the author correct it", async () => {
    await appI18n.changeLanguage("en-US");
    const user = userEvent.setup();
    render(
      <Harness
        node={agentNode({
          schemaVersion: 4,
          completion: JSON.parse('{"files":[null]}'),
        })}
      />,
    );

    const input = screen.getByLabelText("File 1 path");
    expect(input).toHaveValue("");
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(persistedConfig()).toMatchObject({ completion: { files: [null] } });
    await user.type(input, "reports/fixed.md");
    expect(input).toHaveAttribute("aria-invalid", "false");
    expect(persistedConfig()).toMatchObject({
      completion: { files: [{ path: "reports/fixed.md" }] },
    });
  });

  it("marks the agent policy and auto-accept as not applied where the server says so", async () => {
    await appI18n.changeLanguage("en-US");
    render(
      <Harness
        node={agentNode({
          schemaVersion: 4,
          completionSignal: "agent",
          autoAcceptAgentCompletion: true,
        })}
        unappliedSettings={["completionSignal", "autoAcceptAgentCompletion"]}
      />,
    );
    expect(
      screen.getByTestId("workflow-unapplied-completionSignal"),
    ).toHaveTextContent(
      "Saved; v2 has no agent completion tool yet and judges a normally ended turn instead. It takes effect once the completion tool ships.",
    );
    expect(
      screen.getByTestId("workflow-unapplied-autoAcceptAgentCompletion"),
    ).toHaveTextContent(
      "Saved; v2 does not accept completions automatically yet, the starter still confirms. It takes effect once the completion tool ships.",
    );
    expect(screen.queryByTestId("workflow-unapplied-other")).toBeNull();
  });

  it("says in Chinese that the agent policy and auto-accept take effect once the completion tool ships", async () => {
    await appI18n.changeLanguage("zh-CN");
    render(
      <Harness
        node={agentNode({
          schemaVersion: 4,
          completionSignal: "agent",
          autoAcceptAgentCompletion: true,
        })}
        unappliedSettings={["completionSignal", "autoAcceptAgentCompletion"]}
      />,
    );
    expect(
      screen.getByTestId("workflow-unapplied-completionSignal"),
    ).toHaveTextContent(
      "已保存，v2 暂不支持智能体完成工具，运行时按“回合正常结束即可”判定；完成工具上线后会开始生效。",
    );
    expect(
      screen.getByTestId("workflow-unapplied-autoAcceptAgentCompletion"),
    ).toHaveTextContent(
      "已保存，v2 暂不自动接受完成，仍由发起人确认；完成工具上线后会开始生效。",
    );
  });

  it("defaults a node turned interactive to end_turn and clears the policy when turned off", async () => {
    await appI18n.changeLanguage("en-US");
    const user = userEvent.setup();
    render(<Harness node={agentNode({ interactive: false })} />);
    const toggle = screen.getByRole("switch", { name: "Interactive mode" });
    await user.click(toggle);
    expect(persistedConfig()).toMatchObject({
      interactive: true,
      schemaVersion: 4,
      completionSignal: "end_turn",
    });
    expect(screen.getByRole("combobox", { name: "Completion rule" })).toHaveTextContent(
      "A normally ended turn",
    );
    await user.click(toggle);
    expect(persistedConfig()).not.toHaveProperty("completionSignal");
    expect(persistedConfig()).toMatchObject({ interactive: false, schemaVersion: 3 });
  });
});
