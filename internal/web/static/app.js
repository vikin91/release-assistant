document.addEventListener("DOMContentLoaded", () => {
  const sections = Array.from(document.querySelectorAll("details"));
  const sectionKey = (section) => section.dataset.foldKey || section.id;
  const storageKey = "release-assistant:sections";
  let sectionStates = {};
  try {
    const saved = JSON.parse(window.sessionStorage.getItem(storageKey) || "{}");
    if (saved && typeof saved === "object" && !Array.isArray(saved)) sectionStates = saved;
  } catch {}

  const configDraft = document.querySelector("[data-config-draft]") !== null;
  let configSubmitted = false;
  for (const section of sections) {
    const key = sectionKey(section);
    if (key === "release-configuration" && configDraft) continue;
    if (typeof sectionStates[key] === "boolean") section.open = sectionStates[key];
  }
  const saveSections = () => {
    for (const section of sections) {
      const key = sectionKey(section);
      if (key) sectionStates[key] = section.open;
    }
    if (configSubmitted) delete sectionStates["release-configuration"];
    try {
      window.sessionStorage.setItem(storageKey, JSON.stringify(sectionStates));
      return true;
    } catch {
      return false;
    }
  };
  sections.forEach((section) => section.addEventListener("toggle", saveSections));
  window.addEventListener("pagehide", saveSections);
  document.addEventListener("submit", (event) => {
    configSubmitted = event.target.matches(".config-grid");
    saveSections();
  });

  const openSection = (section) => {
    if (section && section.tagName === "DETAILS") {
      section.open = true;
      saveSections();
    }
  };
  document.querySelectorAll("[data-open-section]").forEach((link) => {
    link.addEventListener("click", () => openSection(document.getElementById(link.hash.slice(1))));
  });
  window.addEventListener("hashchange", () => openSection(document.getElementById(window.location.hash.slice(1))));
  const linkedSection = document.getElementById(window.location.hash.slice(1));
  if (linkedSection && !Object.hasOwn(sectionStates, sectionKey(linkedSection))) openSection(linkedSection);

  const activityLog = document.getElementById("activity-log");
  const activityError = document.getElementById("activity-refresh-error");
  if (activityLog && activityError) {
    const scrollKey = "release-assistant:activity-scroll";
    let scrollState = { top: 0, follow: true };
    try {
      const saved = JSON.parse(window.sessionStorage.getItem(scrollKey));
      if (saved && Number.isFinite(saved.top) && saved.top >= 0 && typeof saved.follow === "boolean") scrollState = saved;
    } catch {}
    const restoreActivityScroll = () => {
      if (activityLog.clientHeight > 0) activityLog.scrollTop = scrollState.follow ? activityLog.scrollHeight : scrollState.top;
    };
    const saveActivityScroll = () => {
      if (activityLog.clientHeight === 0) return;
      scrollState = {
        top: activityLog.scrollTop,
        follow: activityLog.scrollHeight - activityLog.scrollTop - activityLog.clientHeight <= 1,
      };
      try { window.sessionStorage.setItem(scrollKey, JSON.stringify(scrollState)); } catch {}
    };
    activityLog.addEventListener("scroll", saveActivityScroll);
    window.addEventListener("pagehide", saveActivityScroll);
    const activityDetails = document.getElementById("activity-details");
    if (activityDetails) activityDetails.addEventListener("toggle", restoreActivityScroll);
    restoreActivityScroll();
    const refreshActivity = async () => {
      try {
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

  document.querySelectorAll("form[data-autosubmit] input[type=checkbox]").forEach((input) => {
    input.addEventListener("change", () => input.form.requestSubmit());
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

  let edited = document.querySelector("[data-config-draft]") !== null;
  document.addEventListener("input", () => { edited = true; }, true);
  document.addEventListener("change", () => { edited = true; }, true);
  const refreshInterval = document.body.dataset.loading === "true" ? 1_000 : 60_000;
  window.setInterval(() => {
    const active = document.activeElement;
    const editing = active && (active.matches("input, textarea, select") || active.isContentEditable);
    if (!edited && !editing && saveSections()) window.location.reload();
  }, refreshInterval);
});
