import {
  useId,
  type InputHTMLAttributes,
  type ReactNode,
  type Ref,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";

// Every governed form (login, intake, plan approval, human decisions,
// revision selection) wrote its own label/input pairing; one (PlanControls'
// "Use deterministic revision" checkbox) silently fell outside the only
// styled context (.intake-form) and rendered unstyled. These primitives are
// the one place field layout, states and label/help/error association live.
export function FieldHelp({ id, children }: { id?: string; children: ReactNode }) {
  return (
    <small id={id} className="muted small">
      {children}
    </small>
  );
}
export function FieldError({ id, children }: { id?: string; children: ReactNode }) {
  return (
    <p id={id} className="field-error" role="alert">
      {children}
    </p>
  );
}
type FieldProps = {
  label: string;
  help?: ReactNode;
  error?: string;
  children: (ids: { id: string; describedBy?: string }) => ReactNode;
};
function Field({ label, help, error, children }: FieldProps) {
  const id = useId();
  const helpId = help ? `${id}-help` : undefined;
  const errorId = error ? `${id}-error` : undefined;
  const describedBy = [helpId, errorId].filter(Boolean).join(" ") || undefined;
  return (
    // Help/error sit as siblings, not inside <label>: a label that wraps a
    // control also folds every other nested text node into that control's
    // computed accessible name - with help nested here the name read out
    // was "Control Plane tokenThe token stays out of URLs..." not just the
    // label (confirmed against the real computed accessible name, not just
    // visually). aria-describedby is the correct channel for help/error.
    <div className="field-label">
      <label htmlFor={id}>{label}</label>
      {children({ id, describedBy })}
      {help && <FieldHelp id={helpId}>{help}</FieldHelp>}
      {error && <FieldError id={errorId}>{error}</FieldError>}
    </div>
  );
}
type FieldCommon = { label: string; help?: ReactNode; error?: string };
export function TextField({
  label,
  help,
  error,
  type = "text",
  ref,
  ...rest
}: FieldCommon & { type?: "text" | "password" | "search"; ref?: Ref<HTMLInputElement> } & Omit<
    InputHTMLAttributes<HTMLInputElement>,
    "id" | "type"
  >) {
  return (
    <Field label={label} help={help} error={error}>
      {({ id, describedBy }) => (
        <input ref={ref} id={id} type={type} aria-describedby={describedBy} aria-invalid={!!error} {...rest} />
      )}
    </Field>
  );
}
export function NumberField({
  label,
  help,
  error,
  ...rest
}: FieldCommon & Omit<InputHTMLAttributes<HTMLInputElement>, "id" | "type">) {
  return (
    <Field label={label} help={help} error={error}>
      {({ id, describedBy }) => (
        <input id={id} type="number" aria-describedby={describedBy} aria-invalid={!!error} {...rest} />
      )}
    </Field>
  );
}
export function SelectField({
  label,
  help,
  error,
  children,
  ...rest
}: FieldCommon & { children: ReactNode } & Omit<SelectHTMLAttributes<HTMLSelectElement>, "id">) {
  return (
    <Field label={label} help={help} error={error}>
      {({ id, describedBy }) => (
        <select id={id} aria-describedby={describedBy} aria-invalid={!!error} {...rest}>
          {children}
        </select>
      )}
    </Field>
  );
}
export function TextareaField({
  label,
  help,
  error,
  ...rest
}: FieldCommon & Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, "id">) {
  return (
    <Field label={label} help={help} error={error}>
      {({ id, describedBy }) => <textarea id={id} aria-describedby={describedBy} aria-invalid={!!error} {...rest} />}
    </Field>
  );
}
export function CheckboxField({
  label,
  ...rest
}: { label: string } & Omit<InputHTMLAttributes<HTMLInputElement>, "id" | "type">) {
  return (
    <label className="checkbox-label">
      <input type="checkbox" {...rest} />
      {label}
    </label>
  );
}
