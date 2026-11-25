document.addEventListener("DOMContentLoaded", () => {
  const topbarResize = "ResizeObserver" in window ? new window.ResizeObserver(([entry]) => {
    document.documentElement.style.setProperty("--topbar-height", `${entry.target.offsetHeight}px`);
  }) : null;
  const topbar = document.querySelector(".topbar");
  if (topbarResize && topbar) {
    document.documentElement.style.setProperty("--topbar-height", `${topbar.offsetHeight}px`);
    topbarResize.observe(topbar);
  }

  const sections = () => Array.from(document.querySelectorAll("details"));
  const sectionKey = (section) => section.dataset.transient !== undefined ? "" : section.dataset.foldKey || section.id;
  const profile = document.body.dataset.profile || "";
  const storageKey = profile ? `release-assistant:sections:${profile}` : "release-assistant:sections";
  const [appSession] = document.body.dataset.session.split(":");
  let sectionStates = {};
  try {
    const saved = JSON.parse(window.sessionStorage.getItem(storageKey));
    if (saved?.session === appSession && saved.sections && typeof saved.sections === "object" && !Array.isArray(saved.sections)) {
      sectionStates = saved.sections;
    }
  } catch {}

  let restoreLogOnOpen;
  let refreshQueueOnOpen;
  let activeTab = document.body.dataset.activeTab || "component-prs";
  const tabURL = (tab) => profile ? `/?release=${encodeURIComponent(profile)}&tab=${tab}` : `/?tab=${tab}`;
  const tabKey = `release-assistant:tab:${appSession}:${profile}`;
  const restoringTab = profile && !new URLSearchParams(window.location.search).has("tab");
  if (restoringTab) {
    try {
      activeTab = window.sessionStorage.getItem(tabKey) || activeTab;
    } catch {}
  }
  const tabLinks = () => Array.from(document.querySelectorAll("[data-workflow-tab]"));
  const activateTab = (tab, historyMode = "") => {
    const links = tabLinks();
    const current = links.find((link) => link.dataset.workflowTab === tab);
    if (!current) return;
    const openingComponent = current.dataset.componentPrTab !== undefined && activeTab !== tab;
    const openingLog = tab === "activity" && activeTab !== tab;
    const openingQueue = tab === "retest-queue" && activeTab !== tab;
    activeTab = tab;
    document.body.dataset.activeTab = tab;
    for (const link of links) {
      const selected = link.dataset.workflowTab === tab;
      link.setAttribute("aria-selected", selected ? "true" : "false");
      link.setAttribute("tabindex", selected ? "0" : "-1");
    }
    for (const panel of document.querySelectorAll("[data-workflow-panel]")) panel.hidden = panel.dataset.workflowPanel !== tab;
    if (openingLog) restoreLogOnOpen?.();
    if (openingQueue) refreshQueueOnOpen?.();
    // Shared forms return to the activity the reader is using.
    for (const input of document.querySelectorAll("#configuration-panel input[name=tab], #retry-settings input[name=tab], .release-activation input[name=tab]")) input.value = tab;
    if (profile) {
      try {
        window.sessionStorage.setItem(tabKey, tab);
      } catch {}
    }
    if (historyMode) window.history[historyMode](null, "", tabURL(tab));
    if (openingComponent && historyMode === "pushState") {
      document.getElementById(current.getAttribute("aria-controls"))?.scrollIntoView?.({ block: "start" });
    }
  };
  const revealTab = (element) => {
    const panel = element?.closest?.("[data-workflow-panel]");
    if (panel && panel.dataset.workflowPanel !== activeTab) {
      const hash = window.location.hash;
      activateTab(panel.dataset.workflowPanel, "replaceState");
      if (hash) window.history.replaceState(null, "", `${tabURL(activeTab)}${hash}`);
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
  if (!tabLinks().some((link) => link.dataset.workflowTab === activeTab)) activeTab = document.body.dataset.activeTab;
  activateTab(activeTab, restoringTab ? "replaceState" : "");

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
    const remove = event.target.closest("[data-confirm-remove]");
    if (remove) {
      document.getElementById(remove.getAttribute("aria-controls")).showModal();
      return;
    }
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
        if (window.history) window.history.replaceState(null, "", `${tabURL(activeTab)}${link.hash}`);
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
      await window.navigator.clipboard.writeText((block.querySelector("[data-copy-value]") || block.querySelector("pre code")).textContent);
      status.textContent = "Copied";
    } catch {
      status.textContent = "Select and copy the text manually.";
    }
    window.clearTimeout(copyTimers.get(status));
    copyTimers.set(status, window.setTimeout(() => { status.textContent = ""; }, 2500));
  });

  const retestSearch = document.getElementById("retest-search");
  const retestFilterKey = `release-assistant:retest-search:${document.body.dataset.session}`;
  let updateRetestSendAll;
  const filterRetests = () => {
    if (!retestSearch) return;
    document.getElementById("retest-filter").hidden = false;
    const query = retestSearch.value.trim().toLowerCase();
    let visible = false;
    for (const bucket of document.querySelectorAll("[data-retest-bucket]")) {
      let visibleBucket = false;
      for (const group of bucket.querySelectorAll("[data-retest-group]")) {
        const targetMatches = group.dataset.retestGroup.toLowerCase().includes(query);
        let visibleGroup = false;
        for (const row of group.querySelectorAll("[data-retest-row]")) {
          row.hidden = !targetMatches && !row.dataset.retestRow.toLowerCase().includes(query);
          visibleGroup ||= !row.hidden;
        }
        group.hidden = !visibleGroup;
        visibleBucket ||= visibleGroup;
      }
      bucket.hidden = !visibleBucket;
      visible ||= visibleBucket;
    }
    const emptySearch = document.getElementById("retest-no-matches");
    if (emptySearch) emptySearch.hidden = visible;
    updateRetestSendAll?.();
  };
  if (retestSearch) {
    try { retestSearch.value = window.sessionStorage.getItem(retestFilterKey) || ""; } catch {}
    retestSearch.addEventListener("input", () => {
      try { window.sessionStorage.setItem(retestFilterKey, retestSearch.value); } catch {}
      filterRetests();
    });
    filterRetests();
  }

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
    window.history.replaceState(null, "", tabURL("settings"));
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
  if (!dashboard || !refreshStatus) {
    if (document.body.dataset.home === "true") {
      const refreshHome = async () => {
        try {
          if (document.hidden || document.querySelector(".release-remove-dialog[open]")) return;
          const response = await fetch("/", { cache: "no-store", signal: AbortSignal.timeout(5_000) });
          if (!response.ok) return;
          const incoming = new DOMParser().parseFromString(await response.text(), "text/html");
          for (const selector of [".topbar", ".release-list", ".workflow-tabs"]) {
            const current = document.querySelector(selector);
            const next = incoming.querySelector(selector);
            if (current && next && !current.contains(document.activeElement) &&
                !current.querySelector("details[open], dialog[open]") && current.innerHTML !== next.innerHTML) {
              current.replaceWith(next);
              if (selector === ".topbar" && topbarResize) {
                topbarResize.unobserve(current);
                topbarResize.observe(next);
              }
            }
          }
        } catch {
          // The next refresh can recover after a brief interruption.
        } finally {
          window.setTimeout(refreshHome, 5_000);
        }
      };
      window.setTimeout(refreshHome, 5_000);
    }
    return;
  }

  const editedControls = new WeakSet();
  if (retestSearch) editedControls.add(retestSearch);
  const rememberEdit = (event) => editedControls.add(event.target);
  document.addEventListener("input", rememberEdit, true);
  document.addEventListener("change", rememberEdit, true);
  let submitting = false;
  let retestRevision = 0;
  const pendingRetests = new Set();
  const eligibleRetestForms = () => Array.from(dashboard.querySelectorAll("[data-retest-send]"))
    .filter((form) => !form.closest("[hidden]") && !form.querySelector("button").disabled);
  updateRetestSendAll = () => {
    const button = document.getElementById("retest-send-all");
    if (!button) return;
    button.hidden = false;
    button.disabled = eligibleRetestForms().length === 0;
  };
  updateRetestSendAll();

  const sendRetests = async (forms) => {
    if (!forms.length) return;
    const data = new URLSearchParams(new FormData(forms[0]));
    data.delete("queue_id");
    const rows = forms.map((form) => {
      const row = form.closest("[data-retest-row]");
      const id = form.querySelector('input[name="queue_id"]').value;
      data.append("queue_id", id);
      pendingRetests.add(row.dataset.liveKey);
      const buttons = Array.from(row.querySelectorAll("button:not(:disabled)"));
      for (const button of buttons) button.disabled = true;
      row.querySelector("[data-retest-sending]").hidden = false;
      row.querySelector("[data-retest-result]").textContent = "";
      return { row, id, buttons };
    });
    updateRetestSendAll();
    let results;
    try {
      const response = await fetch(forms[0].getAttribute("action"), {
        method: "POST", headers: { Accept: "application/json" }, body: data,
      });
      if (!response.ok) throw new Error("Queue request failed");
      results = await response.json();
    } catch {}
    for (const { row, id, buttons } of rows) {
      pendingRetests.delete(row.dataset.liveKey);
      if (results?.[id] === "") {
        row.remove();
      } else {
        row.querySelector("[data-retest-sending]").hidden = true;
        row.querySelector("[data-retest-result]").textContent = results?.[id] ||
          "Could not confirm the result. Check the Queue log before retrying.";
        for (const button of buttons) button.disabled = false;
      }
    }
    retestRevision++;
    filterRetests();
    refreshQueueOnOpen();
  };
  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (form.matches?.("[data-retest-send]")) {
      event.preventDefault();
      if (!form.querySelector("button").disabled) void sendRetests([form]);
    } else {
      submitting = true;
    }
  });
  document.addEventListener("click", (event) => {
    const button = event.target.closest?.("#retest-send-all");
    if (button && !button.disabled) void sendRetests(eligibleRetestForms());
  });
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
    if (current.hasAttribute("data-live-preserve") || pendingRetests.has(current.dataset.liveKey)) return;
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
    const revision = retestRevision;
    try {
      if (document.hidden || submitting) return;
      const response = await fetch(tabURL(activeTab), { cache: "no-store", signal: AbortSignal.timeout(5_000) });
      if (!response.ok) throw new Error("Dashboard request failed");
      const incoming = new DOMParser().parseFromString(await response.text(), "text/html");
      const nextDashboard = incoming.getElementById("live-dashboard");
      if (!nextDashboard) throw new Error("Dashboard response is incomplete");
      if (incoming.body.dataset.session !== document.body.dataset.session) {
        throw new Error("Release configuration changed. Reload the page to continue.");
      }
      if (document.hidden || submitting || revision !== retestRevision) return;
      saveSections();
      const active = document.activeElement;
      const scrollX = window.scrollX;
      const scrollY = window.scrollY;
      patch(document.querySelector("header"), incoming.querySelector("header"));
      patch(dashboard, nextDashboard);
      activateTab(activeTab);
      filterRetests();
      if (active && active.isConnected && document.activeElement !== active) active.focus({ preventScroll: true });
      window.scrollTo(scrollX, scrollY);
      document.body.dataset.loading = incoming.body.dataset.loading;
      document.body.dataset.konfluxHealth = incoming.body.dataset.konfluxHealth ?? "";
      lastUpdated = new Date().toLocaleTimeString();
      refreshStatus.textContent = `Last updated ${lastUpdated}.`;
      refreshStatus.classList.remove("error-text");
    } catch (error) {
      refreshStatus.textContent = error.message === "Release configuration changed. Reload the page to continue."
        ? error.message : `${lastUpdated ? `Last updated ${lastUpdated}. ` : ""}Cannot refresh release data. Showing the last results; retrying automatically.`;
      refreshStatus.classList.add("error-text");
    } finally {
      dashboardRefreshing = false;
      const { loading, konfluxHealth } = document.body.dataset;
      const regularDelay = konfluxHealth === "unverified" ? 30_000 : 60_000;
      const delay = loading === "true" || konfluxHealth === "pending" ? 1_000 : activeTab === "retest-queue" ? 5_000 : regularDelay;
      dashboardTimer = window.setTimeout(refreshDashboard, revision === retestRevision ? delay : 0);
    }
  };
  refreshQueueOnOpen = () => {
    window.clearTimeout(dashboardTimer);
    refreshDashboard();
  };
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {
      window.clearTimeout(dashboardTimer);
      refreshDashboard();
    }
  });
  refreshDashboard();
});
