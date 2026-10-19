"use client";
import { BrandLogo } from "./brand-logo";
import { useRef, useState, useSyncExternalStore } from "react";
import { Icon } from "./ui";
import { getThemeSnapshot, subscribeToTheme, toggleTheme } from "./theme";
import { siteNavigation, repositoryUrl, headerIcons, homeAssets } from "../../shared/header";
import s from "./home.module.css";

function HeaderIcon({ name }: { name: string }) {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={headerIcons[name]} /></svg>;
}
function ThemeToggle() {
  const dark = useSyncExternalStore(subscribeToTheme, getThemeSnapshot, () => false);
  const label = dark ? "切换到浅色" : "切换到深色";
  return <button className="wk-theme-toggle" type="button" role="switch" aria-checked={dark} aria-label={label} title={label} onClick={toggleTheme}><HeaderIcon name={dark ? "moon" : "sun"} /></button>;
}
export function Header() {
  const [open, setOpen] = useState(false);
  const menu = useRef<HTMLButtonElement>(null);
  return <header className="wk-header" onKeyDown={event => { if (event.key === "Escape" && open) { setOpen(false); menu.current?.focus(); } }}>
    <div className="wk-header-inner">
      <a className="wk-brand" href="/" target="_self" aria-label="OPC 首页"><BrandLogo priority /></a>
      <nav id="main-navigation" className={`wk-navigation ${open ? "is-open" : ""}`} aria-label="主导航" onClick={() => setOpen(false)}>
        {siteNavigation.map(item => {
          if (item.children?.length) {
            return (
              <div key={item.href} className="wk-nav-menu">
                <a href={item.href}>{item.label}{item.badge && <span className="wk-new-label">{item.badge}</span>}</a>
                <button type="button" className="wk-nav-caret" aria-label={`${item.label}栏目`}><HeaderIcon name="chevron" /></button>
                <div className="wk-nav-dropdown">
                  {item.children.map(child => <a key={child.href} href={child.href}>{child.label}</a>)}
                </div>
              </div>
            );
          }
          return <a key={item.href} href={item.href}>{item.label}{item.badge && <span className="wk-new-label">{item.badge}</span>}</a>;
        })}
        <a className="wk-mobile-github" href={repositoryUrl} target="_blank" rel="noreferrer">GitHub <HeaderIcon name="external" /></a>
      </nav>
      <ThemeToggle />
      <a className="wk-header-github" href={repositoryUrl} target="_blank" rel="noreferrer"><HeaderIcon name="github" /><span>GitHub</span></a>
      <button ref={menu} type="button" className="wk-menu-toggle" aria-label={open ? "关闭导航" : "打开导航"} aria-expanded={open} aria-controls="main-navigation" onClick={() => setOpen(!open)}><HeaderIcon name={open ? "close" : "menu"} /></button>
    </div>
  </header>;
}
export function ProductVideo() {
  return <figure id="demo" className={s.videoFigure}>
    <div className={s.videoTop}><span>产品界面 <span className={s.videoDot}>/</span> OPC PREVIEW</span><span>知识整理与来源引用</span></div>
    <div className={s.videoStage}>
      <img src={`${homeAssets}/product/agent-chat.png`} alt="OPC 智能体会话：虚构知识整理示例与来源引用" aria-describedby="video-caption" />
    </div>
    <figcaption id="video-caption" className={s.videoCaption}><span>当前 OPC 界面 · 虚构演示资料</span><a href="/docs/01-getting-started/03-quickstart.html">查看快速开始 <Icon name="external" /></a></figcaption>
  </figure>;
}
