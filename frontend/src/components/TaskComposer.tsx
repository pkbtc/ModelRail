import type { FormEvent } from "react";
import { Icon } from "./Icon";

const taskSuggestions = ["Summarize", "Classify", "Extract key points"];

export type TaskComposerSettings = {
  provider: string;
  model: string;
  maxAttempts: string;
  temperature: string;
  maxTokens: string;
  timeoutSeconds: string;
  responseFormat: string;
  jsonSchema: string;
  webhookURL: string;
  webhookSecret: string;
};

type TaskComposerProps = {
  task: string;
  input: string;
  hasKey: boolean;
  providerName: string;
  settings: TaskComposerSettings;
  submitting: boolean;
  onTaskChange: (task: string) => void;
  onInputChange: (input: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  onNeedKey: () => void;
  onSettingsChange: (key: keyof TaskComposerSettings, value: string) => void;
};

export function TaskComposer({
  task,
  input,
  hasKey,
  providerName,
  settings,
  submitting,
  onTaskChange,
  onInputChange,
  onSubmit,
  onNeedKey,
  onSettingsChange,
}: TaskComposerProps) {
  return (
    <section aria-labelledby="composer-title" className="panel composer-panel" id="new-task">
      <div className="panel-heading">
        <div>
          <p className="eyebrow">NEW DISPATCH <span className="eyebrow-index">/ 01</span></p>
          <h2 id="composer-title">Give your task a signal.</h2>
        </div>
        <span className="panel-icon"><Icon name="send" size={17} /></span>
      </div>

      <form onSubmit={onSubmit}>
        <label className="field-label" htmlFor="task-name">Task name</label>
        <input
          className="text-input task-name-input"
          id="task-name"
          maxLength={120}
          onChange={(event) => onTaskChange(event.target.value)}
          placeholder="What should the model do?"
          value={task}
        />
        <div aria-label="Suggested tasks" className="suggestions">
          <span className="suggestions-label">QUICK START</span>
          {taskSuggestions.map((suggestion) => (
            <button className="suggestion-chip" key={suggestion} onClick={() => onTaskChange(suggestion)} type="button">
              {suggestion}
            </button>
          ))}
        </div>

        <div className="input-label-row">
          <label className="field-label" htmlFor="task-input">Input payload</label>
          <span className="field-hint">Up to 1 MB</span>
        </div>
        <textarea
          className="text-input task-textarea"
          id="task-input"
          maxLength={1_000_000}
          onChange={(event) => onInputChange(event.target.value)}
          placeholder="Paste the text, prompt, or context you want to process…"
          value={input}
        />

        <details className="advanced-settings">
          <summary>Model, output, and callback settings</summary>
          <div className="advanced-settings-grid">
            <label className="advanced-field">
              <span>Provider</span>
              <select className="text-input" onChange={(event) => onSettingsChange("provider", event.target.value)} value={settings.provider}>
                <option value="">Server default ({providerName.toLowerCase()})</option>
                <option value="openrouter">OpenRouter</option>
                <option value="mock">Mock</option>
              </select>
            </label>
            <label className="advanced-field">
              <span>Model ID (optional)</span>
              <input className="text-input" onChange={(event) => onSettingsChange("model", event.target.value)} placeholder="openrouter/free" value={settings.model} />
            </label>
            <label className="advanced-field">
              <span>Max attempts (1–10)</span>
              <input className="text-input" max="10" min="1" onChange={(event) => onSettingsChange("maxAttempts", event.target.value)} type="number" value={settings.maxAttempts} />
            </label>
            <label className="advanced-field">
              <span>Timeout seconds</span>
              <input className="text-input" min="1" onChange={(event) => onSettingsChange("timeoutSeconds", event.target.value)} type="number" value={settings.timeoutSeconds} />
            </label>
            <label className="advanced-field">
              <span>Temperature (0–2)</span>
              <input className="text-input" max="2" min="0" onChange={(event) => onSettingsChange("temperature", event.target.value)} step="0.1" type="number" value={settings.temperature} />
            </label>
            <label className="advanced-field">
              <span>Max output tokens</span>
              <input className="text-input" max="32768" min="1" onChange={(event) => onSettingsChange("maxTokens", event.target.value)} placeholder="Provider default" type="number" value={settings.maxTokens} />
            </label>
            <label className="advanced-field">
              <span>Output format</span>
              <select className="text-input" onChange={(event) => onSettingsChange("responseFormat", event.target.value)} value={settings.responseFormat}>
                <option value="">Provider default</option>
                <option value="json_object">JSON object</option>
                <option value="json_schema">JSON schema</option>
              </select>
            </label>
            {settings.responseFormat === "json_schema" && (
              <label className="advanced-field advanced-field-wide">
                <span>JSON Schema</span>
                <textarea className="text-input advanced-schema-input" onChange={(event) => onSettingsChange("jsonSchema", event.target.value)} placeholder={'{"type":"object","properties":{...},"required":[...]}'} value={settings.jsonSchema} />
              </label>
            )}
            <label className="advanced-field advanced-field-wide">
              <span>Completion webhook URL (HTTPS)</span>
              <input autoComplete="url" className="text-input" onChange={(event) => onSettingsChange("webhookURL", event.target.value)} placeholder="https://your-app.example.com/modelrail" value={settings.webhookURL} />
            </label>
            <label className="advanced-field advanced-field-wide">
              <span>Webhook signing secret</span>
              <input autoComplete="new-password" className="text-input" onChange={(event) => onSettingsChange("webhookSecret", event.target.value)} placeholder="At least 16 characters" type="password" value={settings.webhookSecret} />
            </label>
          </div>
          <p className="advanced-settings-note">Webhooks require WEBHOOK_ENCRYPTION_KEY on the ModelRail server. The callback secret is encrypted at rest.</p>
        </details>

        <div className="composer-footer">
          <div className="secure-inline"><Icon name="shield" size={15} /> Private to your instance</div>
          <div className="dispatch-actions">
            <span className="provider-tag"><span className="provider-dot" /> {providerName}</span>
            <button className="button button-primary dispatch-button" disabled={submitting || !task.trim() || !input.trim()} onClick={!hasKey ? onNeedKey : undefined} type={hasKey ? "submit" : "button"}>
              {submitting ? <><span className="button-spinner" /> Queueing…</> : hasKey ? <>Dispatch task <Icon name="arrow-up-right" size={16} /></> : <>Add API key <Icon name="key" size={15} /></>}
            </button>
          </div>
        </div>
      </form>
    </section>
  );
}
