"use strict";
// 后端负责出词、判题、计时和排名；前端只保存用于重绘的猜测记录。
(() => {
  const POLL_MS = 3000;
  const WORD_LEN = 5;
  const { esc, fmtMs } = WordleUI;
  const $ = id => document.getElementById(id);
  const boardEl = $("board");
  const keyEl = $("keyboard");
  const boardWrap = $("boardWrap");

  // token 只存内存，刷新后通过 Keycloak SSO 重新获取。
  const url = new URL(location.href);
  const token = url.searchParams.get("token") || "";
  if (!token) {
    location.href = "/auth/login";
    return;
  }
  url.searchParams.delete("token");
  history.replaceState(null, "", url.pathname + url.search + url.hash);
  $("apiLink").href = "/api.html?token=" + encodeURIComponent(token);

  let roundNo = 0;
  let state = "waiting";
  let guesses = [];
  let current = "";
  let me = { solved: false, attempts: 0 };
  let busy = false;
  let refreshing = false;
  let relogin = false;
  let dismissed = ""; // roundNo:state；状态变化后重新显示提示

  // ---------- 接口 ----------

  async function api(path, opts = {}) {
    const res = await fetch(path, {
      ...opts,
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      const err = new Error(body.error || ("HTTP " + res.status));
      err.status = res.status;
      err.code = body.error;
      throw err;
    }
    return body;
  }

  function onUnauthorized() {
    if (relogin) return;
    relogin = true;
    location.href = "/auth/login";
  }

  // ---------- 渲染 ----------

  function render(scroll = false) {
    const rows = guesses.length + (state === "open" && !me.solved ? 1 : 0);
    boardEl.innerHTML = "";
    for (let r = 0; r < Math.max(rows, 1); r++) {
      const g = guesses[r];
      const word = g ? g.word : current;
      for (let c = 0; c < WORD_LEN; c++) {
        const tile = document.createElement("div");
        tile.className = "tile";
        const ch = word[c] || "";
        if (ch) tile.classList.add("filled");
        if (g) tile.classList.add("hit" + g.result[c]);
        tile.textContent = ch;
        boardEl.appendChild(tile);
      }
    }
    renderKeys();
    renderOverlay();
    renderPill();
    renderMe();
    if (scroll) boardWrap.scrollTop = boardWrap.scrollHeight;
  }

  // 每个字母按最好结果上色：2 绿 > 1 黄 > 0 灰。
  function renderKeys() {
    const best = {};
    for (const g of guesses) {
      for (let i = 0; i < WORD_LEN; i++) {
        const ch = g.word[i];
        const hit = g.result[i];
        if (!(ch in best) || hit > best[ch]) best[ch] = hit;
      }
    }
    for (const key of keyEl.querySelectorAll(".key")) {
      const ch = key.dataset.key;
      key.className = "key" + (key.dataset.wide ? " wide" : "") +
        (ch && ch in best ? " hit" + best[ch] : "");
    }
  }

  function renderOverlay() {
    const overlay = $("overlay");
    overlay.hidden = me.solved || state === "open" || dismissed === roundNo + ":" + state;
    if (overlay.hidden) return;
    $("ovTitle").textContent = state === "waiting" ? "等待本轮开始" : "本轮已结束";
    $("ovText").textContent = state === "waiting"
      ? "管理员开轮后会自动开始，无需刷新页面。（点一下可关掉本提示）"
      : "等待下一轮开始；本轮答案不公布。（点一下可关掉本提示）";
  }

  function renderPill() {
    const pill = $("roundPill");
    pill.textContent = me.solved ? "已解出" :
      state === "waiting" ? "等待开始" : state === "open" ? "进行中" : "已结束";
    pill.className = "pill " + (me.solved ? "solved" : state);
  }

  function renderMe() {
    $("me").innerHTML = me.solved
      ? me.rank
        ? `<div>已解出：第 <b>${me.rank}</b> 名</div><div>步数 <b>${me.attempts}</b> · 用时 <b>${fmtMs(me.elapsedMs)}</b></div>`
        : `<div>已解出，成绩更新中…</div><div>步数 <b>${me.attempts}</b>（本轮）</div>`
      : `<div>步数 <b>${me.attempts}</b>（本轮）</div><div>已解出后进入榜单</div>`;
  }

  function renderBoard(entries) {
    $("lboard").innerHTML = entries.length
      ? entries.map(entry =>
        `<tr class="${entry.rank === me.rank && me.solved ? "mine" : ""}"><td>${entry.rank}</td><td>${esc(entry.displayName)}</td><td>${entry.attempts}</td><td>${fmtMs(entry.elapsedMs)}</td></tr>`
      ).join("")
      : '<tr><td colspan="4" class="muted">还没有人解出</td></tr>';
  }

  let toastTimer;
  function toast(message, ms = 1600) {
    const el = $("toast");
    el.textContent = message;
    el.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.remove("show"), ms);
  }

  // ---------- 本地记录：只用于重绘，不独立保存可推导的解出状态 ----------

  const storeKey = no => "wordle:r:" + no;
  function save() {
    try {
      localStorage.setItem(storeKey(roundNo), JSON.stringify({ guesses }));
    } catch { /* 隐私模式等，不影响比赛 */ }
  }

  function load(no) {
    try {
      const saved = JSON.parse(localStorage.getItem(storeKey(no)));
      if (!Array.isArray(saved?.guesses)) return [];
      return saved.guesses.filter(g => g && /^[a-z]{5}$/.test(g.word) && /^[0-2]{5}$/.test(g.result));
    } catch {
      return [];
    }
  }

  // ---------- 轮次与榜单：顺序轮询，拒绝跨轮或提交前的旧响应 ----------

  function applyRound(no, nextState) {
    if (no === roundNo && nextState === state) return;
    if (no !== roundNo) {
      roundNo = no;
      guesses = load(no);
      current = "";
      me = { solved: guesses.some(g => g.result === "22222"), attempts: guesses.length };
      dismissed = "";
      renderBoard([]);
    }
    state = nextState;
    if (state !== "open") current = "";
    render();
  }

  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      const round = await api("/api/v1/round");
      applyRound(round.roundNo, round.state);
      const attempts = guesses.length;
      const leaderboard = await api("/api/v1/leaderboard");
      if (leaderboard.roundNo !== roundNo || busy || attempts !== guesses.length) return;
      const solvedChanged = me.solved !== leaderboard.me.solved;
      me = leaderboard.me;
      renderBoard(leaderboard.entries);
      if (solvedChanged) render();
      else renderMe();
    } catch (err) {
      if (err.status === 401) onUnauthorized();
      // 其他错误等待下次轮询重试。
    } finally {
      refreshing = false;
    }
  }

  // ---------- 猜词 ----------

  async function submit() {
    if (busy || me.solved) return;
    if (state !== "open") {
      toast(state === "waiting" ? "本轮还没开始" : "本轮已结束");
      return;
    }
    if (current.length !== WORD_LEN) {
      toast("请输入 5 个字母");
      return;
    }
    const word = current;
    const no = roundNo;
    let refreshNeeded = false;
    let justSolved = false;
    busy = true;
    try {
      const response = await api("/api/v1/guesses", {
        method: "POST", body: JSON.stringify({ guess: word }),
      });
      if (no !== roundNo) return;
      guesses.push({ word, result: response.result });
      current = "";
      justSolved = response.result === "22222";
      me = { ...me, solved: justSolved, attempts: me.attempts + 1 };
      save();
      render(true);
      refreshNeeded = justSolved;
    } catch (err) {
      if (err.status === 401) {
        onUnauthorized();
        return;
      }
      if (no !== roundNo) return;
      switch (err.code) {
        case "not_in_word_list": toast("不在词表里"); break;
        case "bad_length": toast("请输入 5 个字母"); break;
        case "already_guessed": toast("这个词已经猜过了"); break;
        case "too_many_requests": toast("太快了，稍等一下"); break;
        case "expired":
        case "already_solved":
        case "round_not_open":
          refreshNeeded = true;
          break;
        default: toast("提交失败：" + err.message);
      }
    } finally {
      busy = false;
    }
    if (refreshNeeded) await refresh();
    if (justSolved && no === roundNo) {
      toast(me.rank
        ? `🎉 第 ${me.rank} 名 · ${me.attempts} 步 · ${fmtMs(me.elapsedMs)}`
        : "🎉 已解出！成绩稍后更新", 4000);
    }
  }

  function type(ch) {
    if (busy || state !== "open" || me.solved || current.length >= WORD_LEN) return;
    current += ch;
    render(true);
  }

  function backspace() {
    if (busy || state !== "open" || me.solved || !current) return;
    current = current.slice(0, -1);
    render(true);
  }

  // ---------- 键盘 ----------

  const ROWS = ["qwertyuiop", "asdfghjkl", "↵zxcvbnm⌫"];
  for (const row of ROWS) {
    const rowEl = document.createElement("div");
    rowEl.className = "krow";
    for (const ch of row) {
      const key = document.createElement("button");
      key.className = "key";
      if (ch === "↵" || ch === "⌫") {
        key.classList.add("wide");
        key.textContent = ch === "↵" ? "enter" : "⌫";
        key.dataset.wide = "1";
        key.onclick = ch === "↵" ? submit : backspace;
      } else {
        key.textContent = ch;
        key.dataset.key = ch;
        key.onclick = () => type(ch);
      }
      rowEl.appendChild(key);
    }
    keyEl.appendChild(rowEl);
  }

  document.addEventListener("keydown", event => {
    if (event.ctrlKey || event.metaKey || event.altKey) return;
    if (event.key === "Enter") submit();
    else if (event.key === "Backspace") backspace();
    else if (/^[a-zA-Z]$/.test(event.key)) type(event.key.toLowerCase());
    else return;
    event.preventDefault();
  });

  $("overlay").addEventListener("click", () => {
    dismissed = roundNo + ":" + state;
    renderOverlay();
  });

  // ---------- 启动 ----------

  render();
  refresh();
  setInterval(refresh, POLL_MS);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) refresh();
  });
})();
