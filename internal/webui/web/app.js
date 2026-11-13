(function () {
  "use strict";

  const stateLabels = {
    queued: "В очереди",
    searching: "Поиск",
    found: "Найден",
    failed: "Ошибка",
    canceled: "Отменён",
  };
  const statDefs = [
    ["total", "Всего"],
    ["queued", "В очереди"],
    ["searching", "Ищутся"],
    ["found", "Найдены"],
    ["failed", "Ошибки"],
    ["canceled", "Отменены"],
  ];

  const els = {
    subtitle: document.getElementById("subtitle"),
    modeBadge: document.getElementById("mode-badge"),
    version: document.getElementById("version"),
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
  let refreshing = false;
  let pendingRefresh = false;
  let toastTimer = 0;

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
      throw new Error("нет связи с magnet-web: " + err.message);
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
    return date.toLocaleString("ru-RU", {
      day: "2-digit",
      month: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function renderInfo(info) {
    if (!info) return;
    const embedded = info.mode === "embedded";
    els.modeBadge.textContent = embedded ? "встроенный движок" : "демон";
    els.modeBadge.className = "badge " + (embedded ? "state-queued" : "state-found");
    if (embedded) {
      els.subtitle.textContent = "Данные: " + (info.data_dir || "—");
    } else {
      els.subtitle.textContent = "Демон: " + (info.server || "—");
    }
    els.version.textContent = info.version ? "v" + info.version : "";
  }

  function renderStats(stats) {
    els.stats.replaceChildren();
    statDefs.forEach(function (definition) {
      const key = definition[0];
      const card = el("div", "stat");
      card.appendChild(el("div", "value", String((stats && stats[key]) || 0)));
      card.appendChild(el("div", "label", definition[1]));
      els.stats.appendChild(card);
    });
  }

  function stateBadge(jobState) {
    const known = Object.prototype.hasOwnProperty.call(stateLabels, jobState);
    const badge = el("span", "badge " + (known ? "state-" + jobState : ""), stateLabels[jobState] || jobState || "—");
    return badge;
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
    const name = el("div", "job-name", job.name || "Без имени");
    name.title = job.magnet || "";
    nameCell.appendChild(name);
    nameCell.appendChild(el("div", "job-hash", shortHash(job.id)));
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
    if (job.state === "found") {
      const link = el("a", "button small", "Скачать");
      link.href = "/api/jobs/" + encodeURIComponent(job.id) + "/torrent";
      actions.appendChild(link);
    }
    if (job.state === "queued" || job.state === "searching") {
      actions.appendChild(actionButton("Отменить", "cancel", job.id));
    }
    if (job.state === "failed" || job.state === "canceled") {
      actions.appendChild(actionButton("Повторить", "retry", job.id));
    }
    actions.appendChild(actionButton("Удалить", "delete", job.id, "danger"));
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
      els.limitNote.textContent = "Показаны последние " + items.length + " из " + total + ". Уточните фильтр.";
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
      showBanner("Не удалось обновить список: " + err.message);
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
      const suffix = els.purge.checked ? " Файл .torrent будет удалён." : " Файл .torrent останется.";
      if (!window.confirm("Удалить задание из списка?" + suffix)) return;
    }
    button.disabled = true;
    try {
      if (action === "cancel") {
        await api("/api/jobs/" + encodeURIComponent(id) + "/cancel", { method: "POST" });
        showToast("Задание отменено", "success");
      } else if (action === "retry") {
        await api("/api/jobs/" + encodeURIComponent(id) + "/retry", { method: "POST" });
        showToast("Задание поставлено в очередь", "success");
      } else if (action === "delete") {
        const query = els.purge.checked ? "?purge=true" : "";
        await api("/api/jobs/" + encodeURIComponent(id) + query, { method: "DELETE" });
        showToast("Задание удалено", "success");
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
      showToast("Вставьте хотя бы одну magnet-ссылку", "error");
      return;
    }
    els.addBtn.disabled = true;
    try {
      const result = await api("/api/jobs", { method: "POST", body: { magnets: links } });
      els.magnets.value = "";
      showToast("Добавлено: " + result.created + ", уже было: " + result.existing, "success");
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
            "Добавлено: " + created + ", уже было: " + existing + ", ошибок: " + failed,
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

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) refresh();
  });

  (async function init() {
    try {
      const info = await api("/api/info");
      renderInfo(info);
    } catch (err) {
      showBanner("Не удалось получить сведения о движке: " + err.message);
    }
    await refresh();
    window.setInterval(refresh, 2000);
  })();
})();
