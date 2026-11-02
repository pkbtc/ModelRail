import { useEffect, useRef, useState, type FormEvent } from "react";
import { Icon } from "./Icon";

type ApiKeyDialogProps = {
  initialValue: string;
  onClose: () => void;
  onSave: (key: string) => void;
};

export function ApiKeyDialog({ initialValue, onClose, onSave }: ApiKeyDialogProps) {
  const [value, setValue] = useState(initialValue);
  const dialogRef = useRef<HTMLElement>(null);

  useEffect(() => {
    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") {
        onClose();
        return;
      }
      if (event.key !== "Tab") return;

      const focusable = dialogRef.current?.querySelectorAll<HTMLElement>(
        'button:not([disabled]), input:not([disabled])',
      );
      if (!focusable?.length) return;

      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }

    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [onClose]);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    onSave(value.trim());
  }

  return (
    <div className="dialog-scrim" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <section
        aria-labelledby="key-dialog-title"
        aria-modal="true"
        className="key-dialog"
        ref={dialogRef}
        role="dialog"
      >
        <button aria-label="Close dialog" className="icon-button dialog-close" onClick={onClose} type="button">
          <Icon name="x" />
        </button>
        <div className="dialog-icon"><Icon name="key" size={21} /></div>
        <p className="eyebrow">ACCESS CONFIGURATION</p>
        <h2 id="key-dialog-title">Connect your workspace</h2>
        <p className="dialog-copy">
          Paste a workspace API key issued by a workspace administrator. It stays in this browser tab and is sent only to this ModelRail instance.
        </p>
        <form onSubmit={handleSubmit}>
          <label className="field-label" htmlFor="api-key">API key</label>
          <input
            autoComplete="off"
            autoFocus
            className="text-input key-input"
            id="api-key"
            onChange={(event) => setValue(event.target.value)}
            placeholder="Paste your bearer key"
            spellCheck={false}
            type="password"
            value={value}
          />
          <div className="secure-note"><Icon name="lock" size={14} /> Stored in session storage. Cleared when this tab session ends.</div>
          <div className="dialog-actions">
            <button className="button button-quiet" onClick={onClose} type="button">Cancel</button>
            <button className="button button-primary" disabled={!value.trim()} type="submit">
              Save key <Icon name="arrow-up-right" size={16} />
            </button>
          </div>
        </form>
      </section>
    </div>
  );
}
