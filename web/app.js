const API = "/api";

async function loadStatus() {
  const res = await fetch(API + "/status");
  const data = await res.json();
  document.getElementById("total").textContent = data.total;
  document.getElementById("pending").textContent = data.pending;
  document.getElementById("done").textContent = data.done;
  document.getElementById("errors").textContent = data.errors;
  document.getElementById("origSize").textContent = fmtBytes(data.original_size);
  document.getElementById("convSize").textContent = fmtBytes(data.output_size);
  const diff = data.output_size - data.original_size;
  document.getElementById("sizeDiff").textContent = (diff >= 0 ? "+" : "-") + fmtBytes(Math.abs(diff));
}

async function loadFiles() {
  const res = await fetch(API + "/files");
  const files = await res.json();
  const tbody = document.getElementById("fileTable");
  tbody.innerHTML = "";
  files.forEach(f => {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${f.path}</td><td>${fmtBytes(f.original_size)}</td><td>${fmtBytes(f.output_size)}</td><td>${f.status}</td><td>${f.error || ""}</td>`;
    tbody.appendChild(tr);
  });
}

async function loadDirs() {
  const res = await fetch(API + "/dirs");
  const dirs = await res.json();
  const list = document.getElementById("dirList");
  list.innerHTML = "";
  dirs.forEach(d => {
    const tag = document.createElement("span");
    tag.className = "tag";
    tag.textContent = d;
    list.appendChild(tag);
  });
}

function fmtBytes(b) {
  if (!b) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(b) / Math.log(k));
  return (b / Math.pow(k, i)).toFixed(1) + " " + sizes[i];
}

document.getElementById("startBtn").addEventListener("click", async () => {
  const workers = parseInt(document.getElementById("workers").value) || 2;
  await fetch(API + "/start", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ workers })
  });
});

document.getElementById("stopBtn").addEventListener("click", async () => {
  await fetch(API + "/stop", { method: "POST" });
});

document.getElementById("scanBtn").addEventListener("click", async () => {
  await fetch(API + "/files?scan=1");
});

document.getElementById("addDirBtn").addEventListener("click", async () => {
  const input = document.getElementById("newDir");
  const dir = input.value.trim();
  if (!dir) return;
  const res = await fetch(API + "/dirs", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify([dir])
  });
  if (res.ok) {
    input.value = "";
    loadDirs();
  }
});

setInterval(() => {
  loadStatus();
}, 2000);
loadDirs();
loadFiles();
