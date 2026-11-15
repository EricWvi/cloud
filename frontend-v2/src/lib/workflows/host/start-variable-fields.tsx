import { parseWorkflowStartDrafts } from './start-variable-drafts'
import type { Dispatch, SetStateAction } from 'react'
import { useTranslation } from 'react-i18next'
import { Checkbox, Input, Label, Select, SelectContent, SelectItem, SelectTrigger, SelectValue, Textarea } from './ui'
import { resolveWorkflowInputFieldType, workflowVariableValueExample, type WorkflowInputVariable } from '../mock'

/** Replaces one field draft without coupling the individual controls to form state shape. */
function updateDraft(
  setDrafts: Dispatch<SetStateAction<Record<string, string>>>,
  name: string,
  value: string,
): void {
  setDrafts((current) => ({ ...current, [name]: value }));
}

/** Shared typed Start-variable controls, using the normal run-start validation and labels. */
export function WorkflowStartVariableFields({variables,drafts,setDrafts,attemptedStart}:{variables:WorkflowInputVariable[];drafts:Record<string,string>;setDrafts:Dispatch<SetStateAction<Record<string,string>>>;attemptedStart:boolean}){
 const {t}=useTranslation();const parsed=parseWorkflowStartDrafts(variables,drafts);
 return <>        {variables.length === 0 ? (
          <p className="rounded-lg bg-muted/45 px-3 py-5 text-center text-xs text-muted-foreground">
            {t("workflowRun.startInputsEmpty")}
          </p>
        ) : (
          variables.map((variable, index) => {
            const parsedVariable = parsed[index];
            const result = parsedVariable?.result;
            const fieldType = resolveWorkflowInputFieldType(variable);
            const inputId = `workflow-run-start-${variable.name}`;
            const value = drafts[variable.name] ?? "";
            const invalid =
              attemptedStart &&
              (result?.valid === false ||
                parsedVariable?.missingRequired === true ||
                parsedVariable?.exceedsMaxLength === true);
            return (
              <div key={variable.name} className="space-y-1.5">
                <div className="flex items-baseline justify-between gap-2">
                  <Label htmlFor={inputId}>
                    {variable.displayName?.trim() || variable.name}
                  </Label>
                  <span className="text-[10px] text-muted-foreground">
                    {variable.required === true
                      ? t("workflowRun.inputRequired")
                      : t("workflowRun.inputOptional")}
                  </span>
                </div>
                {fieldType === "paragraph" ||
                fieldType === "file-list" ||
                fieldType === "json" ? (
                  <Textarea
                    id={inputId}
                    className="min-h-24 bg-muted/55"
                    value={value}
                    aria-invalid={invalid}
                    maxLength={variable.maxLength}
                    placeholder={workflowVariableValueExample(
                      variable.valueType,
                    )}
                    onChange={(event) =>
                      updateDraft(setDrafts, variable.name, event.target.value)
                    }
                  />
                ) : fieldType === "select" ? (
                  <Select
                    value={value || null}
                    onValueChange={(next) =>
                      updateDraft(setDrafts, variable.name, next ?? "")
                    }
                  >
                    <SelectTrigger
                      id={inputId}
                      className="w-full bg-muted/55"
                      aria-invalid={invalid}
                    >
                      <SelectValue
                        placeholder={t("workflowRun.selectPlaceholder")}
                      />
                    </SelectTrigger>
                    <SelectContent>
                      {(variable.options ?? []).map((option) => (
                        <SelectItem key={option} value={option}>
                          {option}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : fieldType === "checkbox" ? (
                  <label
                    htmlFor={inputId}
                    className="flex h-10 items-center gap-2 rounded-md bg-muted/55 px-3 text-sm"
                  >
                    <Checkbox
                      id={inputId}
                      checked={value === "true"}
                      aria-invalid={invalid}
                      onCheckedChange={(checked) =>
                        updateDraft(
                          setDrafts,
                          variable.name,
                          checked === true ? "true" : "false",
                        )
                      }
                    />
                    {value === "true"
                      ? t("settings.workflow.start.checked")
                      : t("settings.workflow.start.unchecked")}
                  </label>
                ) : (
                  <Input
                    id={inputId}
                    className="h-10 bg-muted/55"
                    type={fieldType === "number" ? "number" : "text"}
                    value={value}
                    aria-invalid={invalid}
                    maxLength={variable.maxLength}
                    placeholder={workflowVariableValueExample(
                      variable.valueType,
                    )}
                    onChange={(event) =>
                      updateDraft(setDrafts, variable.name, event.target.value)
                    }
                  />
                )}
                {attemptedStart && result?.valid === false && (
                  <p className="text-[11px] text-destructive" role="status">
                    {t("settings.workflow.variableValueInvalid", {
                      type: variable.valueType,
                    })}
                  </p>
                )}
                {attemptedStart && parsedVariable?.exceedsMaxLength && (
                  <p className="text-[11px] text-destructive" role="status">
                    {t("settings.workflow.start.valueTooLong", {
                      count: variable.maxLength,
                    })}
                  </p>
                )}
                {attemptedStart && parsedVariable?.missingRequired && (
                  <p className="text-[11px] text-destructive" role="status">
                    {t("workflowRun.inputRequiredError")}
                  </p>
                )}
              </div>
            );
          })
        )}</>
}
