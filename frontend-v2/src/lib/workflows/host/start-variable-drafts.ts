import { formatWorkflowVariableValue, parseWorkflowVariableValueText, resolveWorkflowInputFieldType, type WorkflowInputVariable } from '../mock'

/** Initializes typed Start drafts from the frozen snapshot, including checkbox defaults. */
export function workflowStartDrafts(variables:WorkflowInputVariable[]):Record<string,string>{
 return Object.fromEntries(
      variables.map((variable) => [
        variable.name,
        resolveWorkflowInputFieldType(variable) === "checkbox" &&
        variable.value === undefined
          ? "false"
          : formatWorkflowVariableValue(variable.value, variable.valueType),
      ]),
    );
}

/** Parses the same typed fields for both task handling and normal workflow starts. */
export function parseWorkflowStartDrafts(variables:WorkflowInputVariable[],drafts:Record<string,string>){
  return variables.map((variable) => {
    const result = parseWorkflowVariableValueText(
      drafts[variable.name] ?? "",
      variable.valueType,
    );
    return {
      variable,
      result,
      missingRequired:
        variable.required === true &&
        result.valid &&
        result.value === undefined,
      exceedsMaxLength:
        variable.maxLength !== undefined &&
        result.valid &&
        typeof result.value === "string" &&
        Array.from(result.value).length > variable.maxLength,
    };
  });
}
