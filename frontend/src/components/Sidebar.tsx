import { Icon } from "./Icon";

type SidebarProps = {
  onOpenKey: () => void;
  hasKey: boolean;
};

export function Sidebar({ onOpenKey, hasKey }: SidebarProps) {
  return (
    <aside className="sidebar">
      <a aria-label="ModelRail home" className="brand" href="#overview">
        <span className="brand-mark"><span /></span>
        <span className="brand-name">model<span>rail</span></span>
      </a>

      <div className="workspace-switcher">
        <span className="workspace-avatar">M</span>
        <span className="workspace-copy"><strong>My workspace</strong><small>Personal instance</small></span>
        <Icon className="switcher-chevron" name="chevron-down" size={15} />
      </div>

      <p className="nav-label">WORKSPACE</p>
      <nav aria-label="Main navigation" className="side-nav">
        <a className="nav-item active" href="#overview"><Icon name="layers" /> Overview <span className="nav-indicator" /></a>
        <a className="nav-item" href="#activity"><Icon name="activity" /> Runs <span className="nav-count">LIVE</span></a>
      </nav>

      <p className="nav-label nav-label-spaced">DEVELOPER</p>
      <nav aria-label="Developer navigation" className="side-nav">
        <a className="nav-item" href="#new-task"><Icon name="terminal" /> Task console</a>
        <button className="nav-item nav-button" onClick={onOpenKey} type="button"><Icon name="key" /> API credentials <span className={`credential-dot ${hasKey ? "connected" : ""}`} /></button>
      </nav>

      <div className="sidebar-spacer" />

      <div className="sidebar-tip">
        <div className="tip-icon"><Icon name="sparkles" size={17} /></div>
        <strong>Built for clear thinking.</strong>
        <p>Queue work, follow every run, keep the useful bits close.</p>
        <a href="#new-task">Start a task <Icon name="arrow-up-right" size={13} /></a>
      </div>

      <div className="sidebar-footer">
        <div className="avatar-user">P</div>
        <div className="footer-user"><strong>Private instance</strong><small>ModelRail · v1.0</small></div>
        <button aria-label="Configure API key" className="icon-button footer-settings" onClick={onOpenKey} type="button"><Icon name="settings" size={17} /></button>
      </div>
    </aside>
  );
}
