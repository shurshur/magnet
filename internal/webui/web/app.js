(() => {
  "use strict";

  const LANGUAGE_KEY = "magnet-web-language";

  const messages = {
    en: {
      "app.subtitle": "Loading…",
      "language.label": "Language",
      "language.auto": "Auto",
      "add.title": "Add magnet links",
      "add.hint": "One link per line. Lines starting with <code>#</code> are ignored.",
      "add.async": "Searches run in the background: you can add more links without waiting for completion.",
      "add.submit": "Add",
      "filters.label": "Filter by state",
      "filter.all": "All",
      "state.queued": "Queued",
      "state.searching": "Searching",
      "state.found": "Found",
      "state.incomplete": "Incomplete",
      "state.failed": "Failed",
      "state.canceled": "Canceled",
      "purge.label": "delete the .torrent file when deleting a job",
      "table.torrent": "Torrent",
      "table.state": "State",
      "table.attempts": "Attempts",
      "table.size": "Size",
      "table.updated": "Updated",
      "table.actions": "Actions",
      "jobs.empty": "No jobs yet. Add a magnet link above.",
      "jobs.limit": "Showing the latest {shown} of {total}. Narrow the filter.",
      "mode.embedded": "embedded engine",
      "mode.daemon": "daemon",
      "info.data": "Data: {value}",
      "info.server": "Daemon: {value}",
      "stat.total": "Total",
      "stat.queued": "Queued",
      "stat.searching": "Searching",
      "stat.found": "Found",
      "stat.incomplete": "Incomplete",
      "stat.failed": "Failed",
      "stat.canceled": "Canceled",
      "job.unnamed": "Unnamed",
      "action.download": "Download",
      "action.cancel": "Cancel",
      "action.retry": "Retry",
      "action.delete": "Delete",
      "error.connection": "No connection to magnet-web: {error}",
      "error.refresh": "Could not refresh the list: {error}",
      "error.engine": "Could not get engine information: {error}",
      "confirm.delete": "Delete this job from the list?{suffix}",
      "confirm.deleteFile": " The .torrent file will be deleted.",
      "confirm.keepFile": " The .torrent file will be kept.",
      "toast.canceled": "Job canceled",
      "toast.retry": "Job queued for retry",
      "toast.deleted": "Job deleted",
      "toast.needLink": "Paste at least one magnet link",
      "toast.added": "Added: {created}, already existed: {existing}",
      "toast.addedErrors": "Added: {created}, already existed: {existing}, errors: {failed}",
    },
    ru: {
      "app.subtitle": "Загрузка…",
      "language.label": "Язык",
      "language.auto": "Авто",
      "add.title": "Добавить magnet-ссылки",
      "add.hint": "Одна ссылка на строку. Строки, начинающиеся с <code>#</code>, игнорируются.",
      "add.async": "Поиск выполняется в фоне: можно добавлять новые ссылки, не дожидаясь завершения.",
      "add.submit": "Добавить",
      "filters.label": "Фильтр по состоянию",
      "filter.all": "Все",
      "state.queued": "В очереди",
      "state.searching": "Поиск",
      "state.found": "Найден",
      "state.incomplete": "Неполный",
      "state.failed": "Ошибка",
      "state.canceled": "Отменён",
      "purge.label": "удалять .torrent при удалении задания",
      "table.torrent": "Торрент",
      "table.state": "Состояние",
      "table.attempts": "Попытки",
      "table.size": "Размер",
      "table.updated": "Обновлено",
      "table.actions": "Действия",
      "jobs.empty": "Заданий пока нет. Добавьте magnet-ссылку выше.",
      "jobs.limit": "Показаны последние {shown} из {total}. Уточните фильтр.",
      "mode.embedded": "встроенный движок",
      "mode.daemon": "демон",
      "info.data": "Данные: {value}",
      "info.server": "Демон: {value}",
      "stat.total": "Всего",
      "stat.queued": "В очереди",
      "stat.searching": "Ищутся",
      "stat.found": "Найдены",
      "stat.incomplete": "Неполные",
      "stat.failed": "Ошибки",
      "stat.canceled": "Отменены",
      "job.unnamed": "Без имени",
      "action.download": "Скачать",
      "action.cancel": "Отменить",
      "action.retry": "Повторить",
      "action.delete": "Удалить",
      "error.connection": "нет связи с magnet-web: {error}",
      "error.refresh": "Не удалось обновить список: {error}",
      "error.engine": "Не удалось получить сведения о движке: {error}",
      "confirm.delete": "Удалить задание из списка?{suffix}",
      "confirm.deleteFile": " Файл .torrent будет удалён.",
      "confirm.keepFile": " Файл .torrent останется.",
      "toast.canceled": "Задание отменено",
      "toast.retry": "Задание поставлено в очередь",
      "toast.deleted": "Задание удалено",
      "toast.needLink": "Вставьте хотя бы одну magnet-ссылку",
      "toast.added": "Добавлено: {created}, уже было: {existing}",
      "toast.addedErrors": "Добавлено: {created}, уже было: {existing}, ошибок: {failed}",
    },
  };

  const stateLabels = ["queued", "searching", "found", "incomplete", "failed", "canceled"];
  const statDefs = ["total", "queued", "searching", "found", "incomplete", "failed", "canceled"];

  const els = {
    subtitle: document.getElementById("subtitle"),
    modeBadge: document.getElementById("mode-badge"),
    version: document.getElementById("version"),
    languageSelect: document.getElementById("languageSelect"),
    addForm: document.getElementById("add-form"),
    magnets: document.getElementById("magnets"),
    addBtn: document.getElementById("add-btn"),
    filters: document.getElementById("filters"),
    purge: document.getElementById("purge"),
    stats: document.getElementById("stats"),
    banner: document.getElementById("banner"),
    jobs: document.getElementById("jobs"),
    empty: document.getElementById("empty"),
    limitNote: document.getElementById("limit-note"),
    toast: document.getElementById("toast"),
  };

  let state = "";
  let currentInfo = null;
  let refreshing = false;
  let pendingRefresh = false;
  let toastTimer = 0;
  let language = "en";
  let languageChoice = "auto";

  function storedLanguage() {
    try {
      const value = window.localStorage.getItem(LANGUAGE_KEY);
      return value === "ru" || value === "en" ? value : "";
    } catch (err) {
      return "";
    }
  }

  function saveLanguage(value) {
    try {
      if (value) window.localStorage.setItem(LANGUAGE_KEY, value);
      else window.localStorage.removeItem(LANGUAGE_KEY);
    } catch (err) {}
  }

  function detectBrowserLanguage() {
    const languages = (navigator.languages && navigator.languages.length)
      ? navigator.languages
      : [navigator.language || "en"];
    for (const item of languages) {
      const code = String(item).toLowerCase();
      if (code === "ru" || code.startsWith("ru-")) return "ru";
      if (code === "en" || code.startsWith("en-")) return "en";
    }
    return "en";
  }

  function t(key, vars) {
    let text = (messages[language] && messages[language][key]) || messages.en[key] || key;
    if (vars) {
      for (const [name, value] of Object.entries(vars)) {
        text = text.split("{" + name + "}").join(String(value));
      }
    }
    return text;
  }

  function applyStaticTranslations() {
    document.documentElement.lang = language;
    document.title = "magnet";
    document.querySelectorAll("[data-i18n]").forEach(function (node) {
      node.textContent = t(node.dataset.i18n);
    });
    document.querySelectorAll("[data-i18n-html]").forEach(function (node) {
      node.innerHTML = t(node.dataset.i18nHtml);
    });
    document.querySelectorAll("[data-i18n-placeholder]").forEach(function (node) {
      node.placeholder = t(node.dataset.i18nPlaceholder);
    });
    document.querySelectorAll("[data-i18n-aria-label]").forEach(function (node) {
      node.setAttribute("aria-label", t(node.dataset.i18nAriaLabel));
    });
    els.languageSelect.value = languageChoice;
  }

  function setLanguage(choice) {
    languageChoice = choice === "ru" || choice === "en" ? choice : "auto";
    if (languageChoice === "auto") {
      saveLanguage("");
      language = detectBrowserLanguage();
    } else {
      saveLanguage(languageChoice);
      language = languageChoice;
    }
    applyStaticTranslations();
    if (currentInfo) renderInfo(currentInfo);
    refresh();
  }

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  async function api(path, options) {
    const opts = Object.assign({}, options || {});
    opts.headers = Object.assign({}, opts.headers || {});
    if (opts.method && opts.method !== "GET" && opts.method !== "HEAD") {
      opts.headers["X-Magnet-Web"] = "1";
    }
    if (opts.body && typeof opts.body !== "string") {
      opts.body = JSON.stringify(opts.body);
      opts.headers["Content-Type"] = "application/json";
    }
    let response;
    try {
      response = await fetch(path, opts);
    } catch (err) {
      throw new Error(t("error.connection", { error: err.message }));
    }
    const contentType = response.headers.get("content-type") || "";
    let data = null;
    if (contentType.indexOf("application/json") !== -1) {
      data = await response.json().catch(function () { return null; });
    }
    if (!response.ok) {
      const message = data && data.error && data.error.message
        ? data.error.message
        : (response.statusText || ("HTTP " + response.status));
      const error = new Error(message);
      error.status = response.status;
      error.code = data && data.error ? data.error.code : "";
      throw error;
    }
    return data;
  }

  function showToast(text, kind, timeout) {
    els.toast.textContent = text;
    els.toast.className = "toast " + (kind || "info");
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(function () {
      els.toast.className = "toast hidden";
    }, timeout || 5000);
  }

  function showBanner(text) {
    els.banner.textContent = text;
    els.banner.className = "banner";
  }

  function hideBanner() {
    els.banner.className = "banner hidden";
    els.banner.textContent = "";
  }

  function shortHash(id) {
    if (!id) return "";
    return id.length > 16 ? id.slice(0, 16) + "…" : id;
  }

  function formatBytes(value) {
    let n = Number(value) || 0;
    if (n <= 0) return "—";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let index = 0;
    while (n >= 1024 && index < units.length - 1) {
      n /= 1024;
      index++;
    }
    return n.toFixed(index === 0 ? 0 : 1) + " " + units[index];
  }

  function formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "—";
    return date.toLocaleString(language === "ru" ? "ru-RU" : "en-US", {
      day: "2-digit",
      month: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function renderInfo(info) {
    currentInfo = info;
    if (!info) return;
    const embedded = info.mode === "embedded";
    els.modeBadge.textContent = embedded ? t("mode.embedded") : t("mode.daemon");
    els.modeBadge.className = "badge " + (embedded ? "state-queued" : "state-found");
    els.subtitle.textContent = embedded
      ? t("info.data", { value: info.data_dir || "—" })
      : t("info.server", { value: info.server || "—" });
    els.version.textContent = info.version ? "v" + info.version : "";
  }

  function renderStats(stats) {
    els.stats.replaceChildren();
    statDefs.forEach(function (key) {
      const card = el("div", "stat");
      card.appendChild(el("div", "value", String((stats && stats[key]) || 0)));
      card.appendChild(el("div", "label", t("stat." + key)));
      els.stats.appendChild(card);
    });
  }

  function stateLabel(jobState) {
    return stateLabels.indexOf(jobState) !== -1 ? t("state." + jobState) : (jobState || "—");
  }

  function stateBadge(jobState) {
    const known = stateLabels.indexOf(jobState) !== -1;
    return el("span", "badge " + (known ? "state-" + jobState : ""), stateLabel(jobState));
  }

  function actionButton(label, action, id, className) {
    const button = el("button", "small " + (className || ""), label);
    button.type = "button";
    button.dataset.action = action;
    button.dataset.id = id;
    return button;
  }

  function jobRow(job) {
    const row = el("tr");
    const nameCell = el("td");
    const name = el("div", "job-name", job.name || t("job.unnamed"));
    name.title = job.magnet || "";
    nameCell.appendChild(name);
    nameCell.appendChild(el("div", "job-hash", shortHash(job.id) + (job.version ? " · " + job.version : "")));
    if (job.warning) {
      nameCell.appendChild(el("div", "job-warning", job.warning));
    }
    if (job.error) {
      nameCell.appendChild(el("div", "job-error", job.error));
    }

    const stateCell = el("td");
    stateCell.appendChild(stateBadge(job.state));

    const attemptsCell = el("td", "col-meta", String(job.attempts || 0));
    const sizeCell = el("td", "col-meta", formatBytes(job.length));
    const updatedCell = el("td", "col-meta", formatTime(job.updated_at));

    const actionsCell = el("td", "actions-col");
    const actions = el("div", "row-actions");
    if (job.state === "found" || job.state === "incomplete") {
      const link = el("a", "button small", t("action.download"));
      link.href = "/api/jobs/" + encodeURIComponent(job.id) + "/torrent";
      actions.appendChild(link);
    }
    if (job.state === "queued" || job.state === "searching") {
      actions.appendChild(actionButton(t("action.cancel"), "cancel", job.id));
    }
    if (job.state === "failed" || job.state === "canceled" || job.state === "incomplete") {
      actions.appendChild(actionButton(t("action.retry"), "retry", job.id));
    }
    actions.appendChild(actionButton(t("action.delete"), "delete", job.id, "danger"));
    actionsCell.appendChild(actions);

    row.appendChild(nameCell);
    row.appendChild(stateCell);
    row.appendChild(attemptsCell);
    row.appendChild(sizeCell);
    row.appendChild(updatedCell);
    row.appendChild(actionsCell);
    return row;
  }

  function renderJobs(jobs, total) {
    els.jobs.replaceChildren();
    const items = jobs || [];
    items.forEach(function (job) {
      els.jobs.appendChild(jobRow(job));
    });
    els.empty.classList.toggle("hidden", items.length > 0);
    if (total > items.length) {
      els.limitNote.textContent = t("jobs.limit", { shown: items.length, total: total });
      els.limitNote.classList.remove("hidden");
    } else {
      els.limitNote.classList.add("hidden");
      els.limitNote.textContent = "";
    }
  }

  async function refresh() {
    if (refreshing) {
      pendingRefresh = true;
      return;
    }
    if (document.hidden) return;
    refreshing = true;
    try {
      const params = new URLSearchParams({ limit: "500" });
      if (state) params.set("state", state);
      const results = await Promise.all([
        api("/api/jobs?" + params.toString()),
        api("/api/stats"),
      ]);
      renderJobs(results[0].jobs, results[0].total);
      renderStats(results[1].stats);
      hideBanner();
    } catch (err) {
      showBanner(t("error.refresh", { error: err.message }));
    } finally {
      refreshing = false;
      if (pendingRefresh) {
        pendingRefresh = false;
        refresh();
      }
    }
  }

  els.filters.addEventListener("click", function (event) {
    const button = event.target.closest("button[data-state]");
    if (!button) return;
    state = button.dataset.state || "";
    els.filters.querySelectorAll("button").forEach(function (item) {
      item.classList.toggle("active", item === button);
    });
    refresh();
  });

  els.jobs.addEventListener("click", async function (event) {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const action = button.dataset.action;
    const id = button.dataset.id;
    if (!id) return;
    if (action === "delete") {
      const suffix = els.purge.checked ? t("confirm.deleteFile") : t("confirm.keepFile");
      if (!window.confirm(t("confirm.delete", { suffix: suffix }))) return;
    }
    button.disabled = true;
    try {
      if (action === "cancel") {
        await api("/api/jobs/" + encodeURIComponent(id) + "/cancel", { method: "POST" });
        showToast(t("toast.canceled"), "success");
      } else if (action === "retry") {
        await api("/api/jobs/" + encodeURIComponent(id) + "/retry", { method: "POST" });
        showToast(t("toast.retry"), "success");
      } else if (action === "delete") {
        const query = els.purge.checked ? "?purge=true" : "";
        await api("/api/jobs/" + encodeURIComponent(id) + query, { method: "DELETE" });
        showToast(t("toast.deleted"), "success");
      }
      await refresh();
    } catch (err) {
      showToast(err.message, "error", 8000);
      button.disabled = false;
    }
  });

  els.addForm.addEventListener("submit", async function (event) {
    event.preventDefault();
    const links = els.magnets.value
      .split(/\r?\n/)
      .map(function (line) { return line.trim(); })
      .filter(function (line) { return line && !line.startsWith("#"); });
    if (!links.length) {
      showToast(t("toast.needLink"), "error");
      return;
    }
    els.addBtn.disabled = true;
    try {
      const result = await api("/api/jobs", { method: "POST", body: { magnets: links } });
      els.magnets.value = "";
      showToast(t("toast.added", { created: result.created, existing: result.existing }), "success");
      await refresh();
    } catch (err) {
      if (err.status === 400 && links.length > 1) {
        // One malformed line should not reject the whole batch. Retry one by
        // one so valid links are still queued.
        let created = 0;
        let existing = 0;
        let failed = 0;
        let lastError = null;
        for (const link of links) {
          try {
            const single = await api("/api/jobs", { method: "POST", body: { magnets: [link] } });
            created += single.created || 0;
            existing += single.existing || 0;
          } catch (oneErr) {
            failed++;
            lastError = oneErr;
            if (oneErr.status !== 400) break;
          }
        }
        if (created + existing > 0) {
          els.magnets.value = "";
          showToast(
            t("toast.addedErrors", { created: created, existing: existing, failed: failed }),
            failed ? "error" : "success",
            failed ? 9000 : 5000
          );
          await refresh();
        } else {
          showToast(lastError ? lastError.message : err.message, "error", 9000);
        }
      } else {
        showToast(err.message, "error", 9000);
      }
    } finally {
      els.addBtn.disabled = false;
    }
  });

  els.languageSelect.addEventListener("change", function () {
    setLanguage(els.languageSelect.value);
  });

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) refresh();
  });

  (async function init() {
    const saved = storedLanguage();
    languageChoice = saved || "auto";
    language = saved || detectBrowserLanguage();
    applyStaticTranslations();
    try {
      const info = await api("/api/info");
      renderInfo(info);
    } catch (err) {
      showBanner(t("error.engine", { error: err.message }));
    }
    await refresh();
    window.setInterval(refresh, 2000);
  })();
})();
