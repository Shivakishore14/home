let currentFilter = "";
let activeLogTaskID = null;

document.addEventListener("DOMContentLoaded", () => {
  initSSE();
  refreshStats();
  refreshWorkers();
  refreshTasks();

  // Setup tab buttons
  document.querySelectorAll(".tab-btn").forEach(btn => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".tab-btn").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
      currentFilter = btn.dataset.filter || "";
      refreshTasks();
    });
  });

  // Polling fallback every 10s
  setInterval(() => {
    refreshStats();
    refreshWorkers();
  }, 10000);
});

// Real-Time SSE Listener
function initSSE() {
  const evtSource = new EventSource("/api/v1/events");
  const indicator = document.getElementById("sse-dot");
  const label = document.getElementById("sse-label");

  evtSource.onopen = () => {
    indicator.classList.remove("disconnected");
    label.innerText = "Live SSE Connected";
  };

  evtSource.onerror = () => {
    indicator.classList.add("disconnected");
    label.innerText = "Reconnecting...";
  };

  evtSource.addEventListener("connected", () => {
    indicator.classList.remove("disconnected");
    label.innerText = "Live SSE Connected";
  });

  evtSource.addEventListener("task_created", (e) => {
    refreshStats();
    refreshTasks();
    showToast("New task enqueued");
  });

  evtSource.addEventListener("task_claimed", (e) => {
    refreshStats();
    refreshTasks();
  });

  evtSource.addEventListener("task_progress", (e) => {
    const data = JSON.parse(e.data);
    updateTaskProgressRow(data);
  });

  evtSource.addEventListener("task_log", (e) => {
    const data = JSON.parse(e.data);
    if (activeLogTaskID === data.id) {
      appendLogTerminal(data.log_chunk);
    }
  });

  evtSource.addEventListener("task_completed", (e) => {
    refreshStats();
    refreshTasks();
    const data = JSON.parse(e.data);
    showToast(`Task ${data.id.substring(0, 8)} completed`);
  });

  evtSource.addEventListener("task_failed", (e) => {
    refreshStats();
    refreshTasks();
    const data = JSON.parse(e.data);
    showToast(`Task ${data.id.substring(0, 8)} failed`);
  });

  evtSource.addEventListener("task_cancelled", () => {
    refreshStats();
    refreshTasks();
  });

  evtSource.addEventListener("worker_registered", () => {
    refreshWorkers();
    refreshStats();
  });

  evtSource.addEventListener("config_reloaded", () => {
    refreshStats();
    refreshTasks();
    showToast("Configuration reloaded from tasks/");
  });
}

// Fetch & Update Summary Stats
async function refreshStats() {
  try {
    const res = await fetch("/api/v1/stats");
    if (!res.ok) return;
    const stats = await res.json();
    document.getElementById("stat-workers").innerText = stats.active_workers;
    document.getElementById("stat-running").innerText = stats.running_tasks;
    document.getElementById("stat-pending").innerText = stats.pending_tasks;
    document.getElementById("stat-completed").innerText = stats.completed_tasks;
    document.getElementById("stat-failed").innerText = stats.failed_tasks;
  } catch (err) {
    console.error("Failed to fetch stats:", err);
  }
}

// Fetch & Update Active Workers
async function refreshWorkers() {
  try {
    const res = await fetch("/api/v1/workers");
    if (!res.ok) return;
    const workers = await res.json();
    const container = document.getElementById("workers-list");

    if (!workers || workers.length === 0) {
      container.innerHTML = '<div style="color: var(--text-muted); font-size: 0.8rem;">No workers registered yet.</div>';
      return;
    }

    container.innerHTML = workers.map(w => `
      <div class="worker-item">
        <div class="worker-header">
          <span class="worker-id">${escapeHtml(w.id)}</span>
          <span class="worker-status ${w.status.toLowerCase()}">${escapeHtml(w.status)}</span>
        </div>
        <div class="worker-meta">Host: ${escapeHtml(w.hostname)}</div>
        <div class="worker-plugins">
          ${(w.enabled_plugins || []).map(p => `<span class="plugin-tag">${escapeHtml(p)}</span>`).join("")}
        </div>
      </div>
    `).join("");
  } catch (err) {
    console.error("Failed to fetch workers:", err);
  }
}

// Fetch & Render Tasks Table
async function refreshTasks() {
  try {
    let url = "/api/v1/tasks?limit=50";
    if (currentFilter) url += `&status=${encodeURIComponent(currentFilter)}`;
    const res = await fetch(url);
    if (!res.ok) return;
    const tasks = await res.json();
    const tbody = document.getElementById("tasks-tbody");

    if (!tasks || tasks.length === 0) {
      tbody.innerHTML = '<tr><td colspan="7" style="text-align: center; color: var(--text-muted); padding: 2rem;">No tasks found.</td></tr>';
      return;
    }

    tbody.innerHTML = tasks.map(t => {
      const shortID = t.id.substring(0, 8);
      const target = t.target_file ? escapeHtml(t.target_file) : "-";
      const worker = t.worker_id ? escapeHtml(t.worker_id) : "-";
      const progressPct = t.progress ? t.progress.toFixed(1) : 0;
      const speedStr = t.speed ? `(${escapeHtml(t.speed)})` : "";

      return `
        <tr id="task-row-${t.id}">
          <td class="task-id" title="${escapeHtml(t.id)}">${shortID}</td>
          <td><strong>${escapeHtml(t.plugin_name)}</strong></td>
          <td style="max-width: 220px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;" title="${target}">${target}</td>
          <td>${worker}</td>
          <td><span class="badge badge-${t.status.toLowerCase()}">${t.status}</span></td>
          <td>
            <div class="progress-box">
              <div class="progress-bar-bg">
                <div class="progress-bar-fill" id="fill-${t.id}" style="width: ${progressPct}%;"></div>
              </div>
              <div class="progress-info">
                <span id="pct-${t.id}">${progressPct}%</span>
                <span id="speed-${t.id}">${speedStr}</span>
              </div>
            </div>
          </td>
          <td>
            <button class="btn btn-sm" onclick="openLogsModal('${t.id}')">Logs</button>
            ${(t.status === 'PENDING' || t.status === 'RUNNING') ? `
              <button class="btn btn-sm btn-danger" onclick="cancelTask('${t.id}')">Cancel</button>
            ` : ''}
          </td>
        </tr>
      `;
    }).join("");
  } catch (err) {
    console.error("Failed to fetch tasks:", err);
  }
}

// In-place Progress Update for Low Latency
function updateTaskProgressRow(data) {
  const fill = document.getElementById(`fill-${data.id}`);
  const pct = document.getElementById(`pct-${data.id}`);
  const speed = document.getElementById(`speed-${data.id}`);

  if (fill && pct) {
    const p = (data.progress || 0).toFixed(1);
    fill.style.width = `${p}%`;
    pct.innerText = `${p}%`;
    if (speed && data.speed) speed.innerText = `(${data.speed})`;
  }
}

// Reload Configuration
async function reloadConfig() {
  try {
    const res = await fetch("/api/v1/config/reload", { method: "POST" });
    if (res.ok) {
      showToast("Configuration reloaded successfully");
      refreshStats();
      refreshTasks();
    } else {
      const err = await res.json();
      showToast(`Reload error: ${err.error}`);
    }
  } catch (err) {
    showToast(`Failed to trigger reload: ${err.message}`);
  }
}

// Cancel Task
async function cancelTask(taskID) {
  if (!confirm("Are you sure you want to cancel this task?")) return;
  try {
    await fetch(`/api/v1/tasks/${taskID}/cancel`, { method: "POST" });
    showToast("Task cancelled");
    refreshTasks();
    refreshStats();
  } catch (err) {
    showToast("Failed to cancel task");
  }
}

// Modals: Create Task
function openCreateTaskModal() {
  document.getElementById("modal-create-task").classList.add("open");
}

function closeCreateTaskModal() {
  document.getElementById("modal-create-task").classList.remove("open");
}

async function submitCreateTask(e) {
  e.preventDefault();
  const pluginName = document.getElementById("task-plugin").value;
  const targetFile = document.getElementById("task-target").value;
  const priority = parseInt(document.getElementById("task-priority").value, 10) || 0;
  const paramsRaw = document.getElementById("task-params").value || "{}";

  let params = {};
  try {
    params = JSON.parse(paramsRaw);
  } catch (err) {
    alert("Invalid JSON in Parameters field");
    return;
  }

  try {
    const res = await fetch("/api/v1/tasks", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        plugin_name: pluginName,
        target_file: targetFile,
        priority: priority,
        params: params
      })
    });
    if (res.ok) {
      closeCreateTaskModal();
      document.getElementById("form-create-task").reset();
      showToast("Task created successfully");
      refreshTasks();
      refreshStats();
    } else {
      const err = await res.text();
      alert(`Error creating task: ${err}`);
    }
  } catch (err) {
    alert(`Failed to create task: ${err.message}`);
  }
}

// Modals: View Logs
async function openLogsModal(taskID) {
  activeLogTaskID = taskID;
  document.getElementById("modal-logs-title").innerText = `Task Logs: ${taskID.substring(0, 8)}`;
  const term = document.getElementById("terminal-logs");
  term.innerText = "Loading task logs...";
  document.getElementById("modal-logs").classList.add("open");

  try {
    const res = await fetch(`/api/v1/tasks/${taskID}`);
    if (res.ok) {
      const t = await res.json();
      term.innerText = t.log_output || "No logs recorded for this task.";
      term.scrollTop = term.scrollHeight;
    }
  } catch (err) {
    term.innerText = "Failed to load logs.";
  }
}

function closeLogsModal() {
  activeLogTaskID = null;
  document.getElementById("modal-logs").classList.remove("open");
}

function appendLogTerminal(chunk) {
  const term = document.getElementById("terminal-logs");
  if (term.innerText === "Loading task logs..." || term.innerText === "No logs recorded for this task.") {
    term.innerText = "";
  }
  term.innerText += chunk;
  term.scrollTop = term.scrollHeight;
}

// Helper: Toast Notifications
function showToast(msg) {
  const container = document.getElementById("toast-container");
  const toast = document.createElement("div");
  toast.className = "toast";
  toast.innerText = msg;
  container.appendChild(toast);
  setTimeout(() => {
    toast.style.opacity = "0";
    setTimeout(() => toast.remove(), 200);
  }, 3500);
}

function escapeHtml(text) {
  if (!text) return "";
  return String(text)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}
