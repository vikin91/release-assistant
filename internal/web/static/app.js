document.addEventListener("DOMContentLoaded", () => {
  const sections = () => Array.from(document.querySelectorAll("details"));
  const sectionKey = (section) => section.dataset.transient !== undefined ? "" : section.dataset.foldKey || section.id;
  const storageKey = "release-assistant:sections";
  const [appSession] = document.body.dataset.session.split(":");
  let sectionStates = {};
  try {
    const saved = JSON.parse(window.sessionStorage.getItem(storageKey));
    if (saved?.session === appSession && saved.sections && typeof saved.sections === "object" && !Array.isArray(saved.sections)) {
      sectionStates = saved.sections;
    }
  } catch {}

  let restoreLogOnOpen;
  let activeTab = document.body.dataset.activeTab || "component-prs";
  const tabLinks = () => Array.from(document.querySelectorAll("[data-workflow-tab]"));
  const activateTab = (tab, historyMode = "") => {
    const links = tabLinks();
    if (!links.some((link) => link.dataset.workflowTab === tab)) return;
    const openingLog = tab === "activity" && activeTab !== tab;
    activeTab = tab;
    document.body.dataset.activeTab = tab;
    for (const link of links) {
      const selected = link.dataset.workflowTab === tab;
      link.setAttribute("aria-selected", selected ? "true" : "false");
      link.setAttribute("tabindex", selected ? "0" : "-1");
    }
    for (const panel of document.querySelectorAll("[data-workflow-panel]")) panel.hidden = panel.dataset.workflowPanel !== tab;
    if (openingLog) restoreLogOnOpen?.();
    // Shared forms return to the activity the reader is using.
    for (const input of document.querySelectorAll("#configuration-panel input[name=tab], #retry-settings input[name=tab]")) input.value = tab;
    if (historyMode) window.history[historyMode](null, "", `/?tab=${tab}`);
  };
  const revealTab = (element) => {
    const panel = element?.closest?.("[data-workflow-panel]");
    if (panel && panel.dataset.workflowPanel !== activeTab) {
      const hash = window.location.hash;
      activateTab(panel.dataset.workflowPanel, "replaceState");
      if (hash) window.history.replaceState(null, "", `/?tab=${activeTab}${hash}`);
    }
  };
  document.addEventListener("keydown", (event) => {
    const link = event.target.closest("[data-workflow-tab]");
    if (!link || !["ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    const links = tabLinks();
    const index = links.indexOf(link);
    const next = event.key === "Home" ? 0 : event.key === "End" ? links.length - 1 : (index + (["ArrowRight", "ArrowDown"].includes(event.key) ? 1 : -1) + links.length) % links.length;
    event.preventDefault();
    activateTab(links[next].dataset.workflowTab, "pushState");
    links[next].focus();
  });
  window.addEventListener("popstate", () => activateTab(new URLSearchParams(window.location.search).get("tab") || (document.body.dataset.configured === "false" ? "settings" : "component-prs")));
  activateTab(activeTab);

  const restoreSection = (section) => {
    const key = sectionKey(section);
    if (typeof sectionStates[key] === "boolean") section.open = sectionStates[key];
  };
  sections().forEach(restoreSection);
  const saveSections = () => {
    for (const section of sections()) {
      const key = sectionKey(section);
      if (key) sectionStates[key] = section.open;
    }
    try {
      window.sessionStorage.setItem(storageKey, JSON.stringify({ session: appSession, sections: sectionStates }));
      return true;
    } catch {
      return false;
    }
  };
  document.addEventListener("toggle", saveSections, true);
  window.addEventListener("pagehide", saveSections);
  document.addEventListener("submit", saveSections);

  const openSection = (section) => {
    revealTab(section);
    if (section && section.tagName === "DETAILS") {
      section.open = true;
      saveSections();
    }
  };
  document.addEventListener("click", (event) => {
    const tab = event.target.closest("[data-workflow-tab]");
    if (tab?.dataset?.workflowTab && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey && (!event.button || event.button === 0)) {
      event.preventDefault();
      activateTab(tab.dataset.workflowTab, "pushState");
      const drawer = tab.closest(".sidebar:popover-open");
      if (drawer) {
        drawer.hidePopover();
        document.getElementById(tab.getAttribute("aria-controls")).focus();
      }
      return;
    }
    const link = event.target.closest("[data-open-section]");
    if (link && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey) {
      const section = document.getElementById(link.hash.slice(1));
      if (section) {
        const menu = link.closest?.("details.header-menu");
        if (menu) menu.open = false;
        openSection(section);
        event.preventDefault();
        if (window.history) window.history.replaceState(null, "", `/?tab=${activeTab}${link.hash}`);
        section.scrollIntoView?.({ block: "start" });
      }
    }
  });
  window.addEventListener("hashchange", () => openSection(document.getElementById(window.location.hash.slice(1))));
  const linkedSection = document.getElementById(window.location.hash.slice(1));
  revealTab(linkedSection);
  if (linkedSection && !Object.hasOwn(sectionStates, sectionKey(linkedSection))) openSection(linkedSection);

  document.body.dataset.copyEnabled = "true";
  const copyTimers = new WeakMap();
  document.addEventListener("click", async (event) => {
    const button = event.target.closest?.("[data-copy-text]");
    if (button?.dataset?.copyText === undefined) return;
    const block = button.closest(".copyable-text");
    const status = block.querySelector("[data-copy-status]");
    window.clearTimeout(copyTimers.get(status));
    try {
      await window.navigator.clipboard.writeText(block.querySelector("pre code").textContent);
      status.textContent = "Copied";
    } catch {
      status.textContent = "Select and copy the text manually.";
    }
    window.clearTimeout(copyTimers.get(status));
    copyTimers.set(status, window.setTimeout(() => { status.textContent = ""; }, 2500));
  });

  const activityLog = document.getElementById("activity-log");
  const activityError = document.getElementById("activity-refresh-error");
  const jumpToLatest = document.getElementById("activity-jump-latest");
  if (activityLog && activityError) {
    const scrollKey = "release-assistant:activity-scroll";
    let scrollState = { top: 0, follow: true };
    try {
      const saved = JSON.parse(window.sessionStorage.getItem(scrollKey));
      if (saved && Number.isFinite(saved.top) && saved.top >= 0 && typeof saved.follow === "boolean") scrollState = saved;
    } catch {}
    const updateActivityJump = () => {
      if (jumpToLatest) jumpToLatest.hidden = scrollState.follow || activityLog.clientHeight === 0;
    };
    const restoreActivityScroll = () => {
      if (activityLog.clientHeight > 0) activityLog.scrollTop = scrollState.follow ? activityLog.scrollHeight : scrollState.top;
      updateActivityJump();
    };
    const saveActivityScroll = () => {
      if (activityLog.clientHeight === 0) return;
      scrollState = {
        top: activityLog.scrollTop,
        follow: activityLog.scrollHeight - activityLog.scrollTop - activityLog.clientHeight <= 1,
      };
      updateActivityJump();
      try { window.sessionStorage.setItem(scrollKey, JSON.stringify(scrollState)); } catch {}
    };
    jumpToLatest?.addEventListener("click", () => {
      activityLog.scrollTop = activityLog.scrollHeight;
      activityLog.focus({ preventScroll: true });
      saveActivityScroll();
    });
    activityLog.addEventListener("scroll", saveActivityScroll);
    window.addEventListener("pagehide", saveActivityScroll);
    restoreLogOnOpen = restoreActivityScroll;
    restoreActivityScroll();
    const refreshActivity = async () => {
      try {
        if (document.hidden) return;
        const response = await fetch("/activity", { cache: "no-store", signal: AbortSignal.timeout(5_000) });
        if (!response.ok) throw new Error("Activity request failed");
        const text = await response.text();
        if (activityLog.textContent !== text) {
          saveActivityScroll();
          activityLog.textContent = text;
          restoreActivityScroll();
        }
        activityError.hidden = true;
      } catch {
        activityError.textContent = "Cannot refresh the activity log. Check whether the assistant is still running.";
        activityError.hidden = false;
      } finally {
        window.setTimeout(refreshActivity, 2_000);
      }
    };
    refreshActivity();
  }

  document.addEventListener("change", (event) => {
    if (event.target.matches("form[data-autosubmit] input[type=checkbox]")) event.target.form.requestSubmit();
  });

  const version = document.querySelector('input[name="version"]');
  const scanner = document.querySelector("[data-scanner-fields]");
  if (version && scanner) {
    const update = () => {
      const value = version.value.trim();
      scanner.hidden = value !== "" && !value.startsWith("4.");
    };
    version.addEventListener("input", update);
    update();
  }

  document.querySelectorAll("[data-component-config]").forEach((component) => {
    const repo = component.querySelector('input[name$="_repo"]');
    const stream = component.querySelector("[data-release-stream]");
    const branch = component.querySelector("[data-release-branch]");
    const configured = component.dataset;
    const update = () => {
      branch.value = configured.configuredBranch && repo.value === configured.configuredRepo &&
        (stream.value === configured.configuredStream || configured.configuredStream === "")
        ? configured.configuredBranch
        : stream.value ? `release-${stream.value}` : "";
    };
    repo.addEventListener("input", update);
    stream.addEventListener("input", update);
  });

  const configForm = document.querySelector("form[data-saved-config]");
  const confirmation = document.getElementById("configuration-saved");
  if (confirmation && configForm) {
    window.history.replaceState(null, "", "/?tab=settings");
    configForm.addEventListener("input", () => confirmation.remove(), { once: true });
  }
  const setupState = document.getElementById("setup-state");
  if (configForm && setupState && document.body.dataset.configured === "true") {
    const saved = new URLSearchParams(configForm.dataset.savedConfig);
    const fields = Array.from(configForm.querySelectorAll("input:not([type=hidden]):not([readonly])"));
    const updateSetupState = () => {
      setupState.hidden = fields.every(field => (field.type === "checkbox" && !field.checked ? "" : field.value) === (saved.get(field.name) || ""));
    };
    configForm.addEventListener("input", updateSetupState);
    configForm.addEventListener("change", updateSetupState);
    updateSetupState();
  }

  const dashboard = document.getElementById("live-dashboard");
  const refreshStatus = document.getElementById("dashboard-refresh-status");
  if (!dashboard || !refreshStatus) return;

  const editedControls = new WeakSet();
  const rememberEdit = (event) => editedControls.add(event.target);
  document.addEventListener("input", rememberEdit, true);
  document.addEventListener("change", rememberEdit, true);
  let submitting = false;
  document.addEventListener("submit", () => { submitting = true; });
  window.addEventListener("pageshow", () => { submitting = false; });

  const nodeKey = (node) => {
    if (node.nodeType !== 1) return "";
    const id = node.getAttribute("id");
    if (id || node.dataset.liveKey || node.dataset.foldKey) return id || node.dataset.liveKey || node.dataset.foldKey;
    if (node.tagName === "INPUT") return `${node.type}:${node.name}`;
    // Form identity binds local edits to the exact target and proposed action.
    if (node.tagName === "FORM") return JSON.stringify(Array.from(node.querySelectorAll('input[type="hidden"]'))
      .filter((input) => !["csrf", "generation", "tab"].includes(input.name))
      .map((input) => [input.name, input.value]));
    return "";
  };
  const patch = (current, incoming) => {
    if (current.nodeType !== 1) {
      if (current.nodeValue !== incoming.nodeValue) current.nodeValue = incoming.nodeValue;
      return;
    }
    if (current.hasAttribute("data-live-preserve")) return;
    const keepInput = current.tagName === "INPUT" && editedControls.has(current) && !incoming.disabled;
    const value = current.value;
    const checked = current.checked;
    for (const attribute of Array.from(current.attributes)) {
      if (current.tagName === "DETAILS" && attribute.name === "open") continue;
      if (!incoming.hasAttribute(attribute.name)) current.removeAttribute(attribute.name);
    }
    for (const attribute of incoming.attributes) {
      if (current.tagName === "DETAILS" && attribute.name === "open") continue;
      if (current.getAttribute(attribute.name) !== attribute.value) current.setAttribute(attribute.name, attribute.value);
    }
    if (current.tagName === "INPUT") {
      current.checked = keepInput ? checked : incoming.checked;
      const nextValue = keepInput ? value : incoming.value;
      if (current.value !== nextValue) current.value = nextValue;
    }
    const keyed = new Map(Array.from(current.childNodes).filter(nodeKey).map((node) => [nodeKey(node), node]));
    let cursor = current.firstChild;
    for (const next of incoming.childNodes) {
      const key = nodeKey(next);
      let match = key ? keyed.get(key) : cursor;
      if (match && (match.nodeType !== next.nodeType || match.nodeName !== next.nodeName || nodeKey(match) !== key)) match = null;
      if (!match) {
        match = next.cloneNode(true);
        if (match.nodeType === 1) {
          if (match.tagName === "DETAILS") restoreSection(match);
          match.querySelectorAll("details").forEach(restoreSection);
        }
        current.insertBefore(match, cursor);
      } else {
        if (match !== cursor) current.insertBefore(match, cursor);
        patch(match, next);
      }
      cursor = match.nextSibling;
    }
    while (cursor) {
      const removed = cursor;
      cursor = cursor.nextSibling;
      removed.remove();
    }
  };

  let dashboardRefreshing = false;
  let dashboardTimer;
  let lastUpdated = "";
  const refreshDashboard = async () => {
    if (dashboardRefreshing) return;
    dashboardRefreshing = true;
    try {
      if (document.hidden || submitting) return;
      const response = await fetch(`/?tab=${activeTab}`, { cache: "no-store", signal: AbortSignal.timeout(5_000) });
      if (!response.ok) throw new Error("Dashboard request failed");
      const incoming = new DOMParser().parseFromString(await response.text(), "text/html");
      const nextDashboard = incoming.getElementById("live-dashboard");
      if (!nextDashboard) throw new Error("Dashboard response is incomplete");
      if (incoming.body.dataset.session !== document.body.dataset.session) {
        throw new Error("Release configuration changed. Reload the page to continue.");
      }
      if (document.hidden || submitting) return;
      saveSections();
      const active = document.activeElement;
      const scrollX = window.scrollX;
      const scrollY = window.scrollY;
      patch(document.querySelector("header"), incoming.querySelector("header"));
      patch(dashboard, nextDashboard);
      activateTab(activeTab);
      if (active && active.isConnected && document.activeElement !== active) active.focus({ preventScroll: true });
      window.scrollTo(scrollX, scrollY);
      document.body.dataset.loading = incoming.body.dataset.loading;
      lastUpdated = new Date().toLocaleTimeString();
      refreshStatus.textContent = `Last updated ${lastUpdated}.`;
      refreshStatus.classList.remove("error-text");
    } catch (error) {
      refreshStatus.textContent = error.message === "Release configuration changed. Reload the page to continue."
        ? error.message : `${lastUpdated ? `Last updated ${lastUpdated}. ` : ""}Cannot refresh release data. Showing the last results; retrying automatically.`;
      refreshStatus.classList.add("error-text");
    } finally {
      dashboardRefreshing = false;
      dashboardTimer = window.setTimeout(refreshDashboard, document.body.dataset.loading === "true" ? 1_000 : 60_000);
    }
  };
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {
      window.clearTimeout(dashboardTimer);
      refreshDashboard();
    }
  });
  refreshDashboard();
});
