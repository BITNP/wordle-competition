"use strict";
// 后端负责出词、判题、计时和排名；前端只保存用于重绘的猜测记录。
//
// 两种玩法共用这一份脚本，差别只在收工条件与榜单口径：
//   round 轮次赛：一轮一题，解出即收工，榜单按步数 / 用时排；
//   timed 限时赛：限时内答对一题立刻换下一题继续答，榜单按答对题数排。
(() => {
  const POLL_MS = 3000;
  const WORD_LEN = 5;
  const CLEAR_MS = 1200; // 限时赛答对后，留住刚解出的那行给玩家看一眼，再清盘换题
  const { esc, fmtMs, fmtLeft } = WordleUI;
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
  let mode = "round";
  let state = "waiting";
  let endsAt = 0;   // 限时赛截止时刻（服务端时钟）
  let offset = 0;   // 服务端时钟 - 本地时钟，倒计时和截止判定都用校正后的时间
  let guesses = []; // 当前题的猜测记录
  let current = "";
  let seq = 1;         // 限时赛当前是第几题，仅用于本地存档分桶
  let localSolved = 0; // 本地已知的答对题数，用来发现"漏了一次答对"
  let me = { solved: false, attempts: 0, solvedCount: 0, totalAttempts: 0 };
  let busy = false;
  let clearing = false; // 限时赛清盘换题的动画中，此间不接受输入
  let refreshing = false;
  let relogin = false;
  let expiredPolled = false; // 倒计时到点后只立刻补拉一次状态
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

  const serverTime = () => Date.now() + offset;
  const playing = () => state === "open" && !(mode === "round" && me.solved);

  // ---------- 渲染 ----------

  function render(scroll = false) {
    // 清盘换题的动画期间不再补当前输入行，否则已解出的那行下面会闪一个空格子。
    const rows = guesses.length + (playing() && !clearing ? 1 : 0);
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
    renderHud();
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
    // 轮次赛解出即收工，玩家在看自己的棋盘，不再盖遮罩（保持原有行为）；
    // 限时赛只有"还没开始"和"时间到"两种情况需要遮挡。
    const show = !(mode === "round" && me.solved) && (state === "waiting" || state === "closed");
    overlay.hidden = !show || dismissed === roundNo + ":" + state;
    if (overlay.hidden) return;
    if (state === "waiting") {
      $("ovTitle").textContent = "等待本轮开始";
      $("ovText").textContent = "管理员开赛后会自动开始，无需刷新页面。（点一下可关掉本提示）";
    } else if (mode === "timed") {
      $("ovTitle").textContent = "时间到";
      $("ovText").textContent = "本场共答对 " + (me.solvedCount || 0) + " 题，名次看榜单。（点一下可关掉本提示）";
    } else {
      $("ovTitle").textContent = "本轮已结束";
      $("ovText").textContent = "等待下一轮开始；本轮答案不公布。（点一下可关掉本提示）";
    }
  }

  function renderPill() {
    const pill = $("roundPill");
    const done = mode === "round" && me.solved;
    pill.textContent = done ? "已解出"
      : state === "waiting" ? "等待开始"
      : state === "closed" ? "已结束"
      : mode === "timed" ? "限时赛进行中" : "进行中";
    pill.className = "pill " + (done ? "solved" : state);
  }

  function renderMe() {
    if (mode === "timed") {
      const rank = me.rank ? "第 <b>" + me.rank + "</b> 名" : "排名更新中…";
      $("me").innerHTML =
        `<div>已答对 <b>${me.solvedCount || 0}</b> 题</div>` +
        `<div>累计步数 <b>${me.totalAttempts || 0}</b> · 当前题 <b>${me.attempts || 0}</b> 步</div>` +
        `<div>${me.solvedCount ? rank : "答对一题即可上榜"}</div>`;
      return;
    }
    $("me").innerHTML = me.solved
      ? me.rank
        ? `<div>已解出：第 <b>${me.rank}</b> 名</div><div>步数 <b>${me.attempts}</b> · 用时 <b>${fmtMs(me.elapsedMs)}</b></div>`
        : `<div>已解出，成绩更新中…</div><div>步数 <b>${me.attempts}</b>（本轮）</div>`
      : `<div>步数 <b>${me.attempts}</b>（本轮）</div><div>已解出后进入榜单</div>`;
  }

  // 限时赛顶部信息条：已答对题数 + 倒计时。
  function renderHud() {
    const timed = mode === "timed";
    $("hud").hidden = !timed;
    if (timed) $("hudSolved").textContent = me.solvedCount || 0;
    tick();
  }

  function tick() {
    if (mode !== "timed") return;
    if (!endsAt) {
      $("hudLeft").textContent = "--:--";
      return;
    }
    const left = endsAt - serverTime();
    $("hudLeft").textContent = fmtLeft(left);
    // 本地先看到 0:00 就立刻补拉一次状态，不用等服务端轮询周期。
    if (left <= 0 && state === "open" && !expiredPolled) {
      expiredPolled = true;
      refresh();
    }
  }

  function renderBoard(entries) {
    const timed = mode === "timed";
    $("boardTitle").textContent = timed ? "实时排行" : "本轮榜单";
    $("lbHead").innerHTML = timed
      ? "<tr><th>名次</th><th>昵称</th><th>答对</th><th>步数</th></tr>"
      : "<tr><th>名次</th><th>昵称</th><th>步数</th><th>用时</th></tr>";
    $("lboard").innerHTML = entries.length
      ? entries.map(entry => {
        const mine = entry.rank === me.rank && me.solvedCount > 0 ? "mine" : "";
        return timed
          ? `<tr class="${mine}"><td>${entry.rank}</td><td>${esc(entry.displayName)}</td><td><b>${entry.solvedCount}</b></td><td>${entry.attempts}</td></tr>`
          : `<tr class="${mine}"><td>${entry.rank}</td><td>${esc(entry.displayName)}</td><td>${entry.attempts}</td><td>${fmtMs(entry.elapsedMs)}</td></tr>`;
      }).join("")
      : `<tr><td colspan="4" class="muted">${timed ? "还没有人答对" : "还没有人解出"}</td></tr>`;
  }

  let toastTimer;
  function toast(message, ms = 1600) {
    const el = $("toast");
    el.textContent = message;
    el.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.remove("show"), ms);
  }

  // ---------- 本地记录：只用于重绘，名次与答对题数一律以服务端为准 ----------

  const storeKey = no => "wordle:r:" + no;
  function save() {
    try {
      localStorage.setItem(storeKey(roundNo), JSON.stringify({ seq, guesses, solvedCount: localSolved }));
    } catch { /* 隐私模式等，不影响比赛 */ }
  }

  // 兼容两种存档：早期版本只存 guesses 数组，现在存 {seq, guesses, solvedCount}。
  function load(no) {
    try {
      const saved = JSON.parse(localStorage.getItem(storeKey(no)));
      const raw = Array.isArray(saved) ? saved : saved?.guesses;
      const list = Array.isArray(raw)
        ? raw.filter(g => g && /^[a-z]{5}$/.test(g.word) && /^[0-2]{5}$/.test(g.result))
        : [];
      return {
        guesses: list,
        seq: Number.isInteger(saved?.seq) && saved.seq > 0 ? saved.seq : 1,
        solvedCount: Number.isInteger(saved?.solvedCount) && saved.solvedCount > 0 ? saved.solvedCount : 0,
      };
    } catch {
      return { guesses: [], seq: 1, solvedCount: 0 };
    }
  }

  // ---------- 场次与榜单：顺序轮询，拒绝跨场或提交前的旧响应 ----------

  function applyRound(round) {
    offset = round.serverNow ? round.serverNow - Date.now() : offset;
    endsAt = round.endsAt || 0;
    const no = round.roundNo;
    const nextState = round.state;
    const nextMode = round.mode || "round";
    if (no !== roundNo) {
      const saved = load(no);
      roundNo = no;
      seq = saved.seq;
      localSolved = saved.solvedCount;
      guesses = saved.guesses;
      current = "";
      expiredPolled = false;
      dismissed = "";
      me = {
        solved: nextMode === "round" && guesses.some(g => g.result === "22222"),
        attempts: guesses.length,
        solvedCount: 0,
        totalAttempts: 0,
      };
      renderBoard([]);
    }
    mode = nextMode;
    state = nextState;
    if (mode !== "timed" && state !== "open") current = "";
    render();
  }

  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      const round = await api("/api/v1/round");
      applyRound(round);
      const attempts = guesses.length;
      const leaderboard = await api("/api/v1/leaderboard");
      if (leaderboard.roundNo !== roundNo || busy || clearing || attempts !== guesses.length) return;
      me = leaderboard.me;
      // 本地漏过一次答对（提交成功但响应丢了 / 中途刷新）时，服务端的题数更大：
      // 按服务端对齐，丢掉属于上一题的棋盘，直接开新题，免得旧提示被当成本题线索。
      if (mode === "timed" && me.solvedCount > localSolved) {
        localSolved = me.solvedCount;
        seq = localSolved + 1;
        guesses = [];
        current = "";
        save();
      }
      renderBoard(leaderboard.entries);
      render();
    } catch (err) {
      if (err.status === 401) onUnauthorized();
      // 其他错误等待下次轮询重试。
    } finally {
      refreshing = false;
    }
  }

  // ---------- 猜词 ----------

  // 限时赛答对后换题：留住刚解出的那行 CLEAR_MS，再清空棋盘。
  function clearForNextWord(no) {
    return new Promise(resolve => {
      clearing = true;
      setTimeout(() => {
        if (no === roundNo) {
          guesses = [];
          seq = localSolved + 1;
          current = "";
          save();
          render();
        }
        clearing = false;
        resolve();
      }, CLEAR_MS);
    });
  }

  async function submit() {
    if (busy || clearing) return;
    if (mode === "round" && me.solved) return;
    if (state !== "open") {
      toast(state === "waiting" ? "本轮还没开始" : state === "closed" && mode === "timed" ? "时间到，本场已结束" : "本轮已结束");
      return;
    }
    if (current.length !== WORD_LEN) {
      toast("请输入 5 个字母");
      return;
    }
    const word = current;
    const no = roundNo;
    let refreshNeeded = false;
    let solved = false;
    let usedAttempts = 0;
    busy = true;
    try {
      const response = await api("/api/v1/guesses", {
        method: "POST", body: JSON.stringify({ guess: word }),
      });
      if (no !== roundNo) return;
      guesses.push({ word, result: response.result });
      current = "";
      solved = response.solved === true;
      usedAttempts = response.attempts;
      me = {
        ...me,
        solved: mode === "round" ? solved : false,
        solvedCount: Number.isInteger(response.solvedCount) ? response.solvedCount : me.solvedCount,
        // 限时赛答对即换题：本题已结束，新题从 0 步起算。
        attempts: solved && mode === "timed" ? 0 : response.attempts,
        totalAttempts: (me.totalAttempts || 0) + 1,
      };
      if (solved) localSolved = me.solvedCount;
      // 先挡住输入、也别补空格子，再重绘出刚解出的那一行。
      if (solved && mode === "timed") clearing = true;
      save();
      render(true);
      if (solved && mode === "timed") await clearForNextWord(no);
      refreshNeeded = solved;
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
    if (solved) {
      toast(mode === "timed"
        ? `🎉 答对第 ${me.solvedCount} 题 · 本题 ${usedAttempts} 步`
        : me.rank
          ? `🎉 第 ${me.rank} 名 · ${me.attempts} 步 · ${fmtMs(me.elapsedMs)}`
          : "🎉 已解出！成绩稍后更新", 4000);
    }
  }

  function type(ch) {
    if (busy || clearing || state !== "open" || (mode === "round" && me.solved) || current.length >= WORD_LEN) return;
    current += ch;
    render(true);
  }

  function backspace() {
    if (busy || clearing || state !== "open" || (mode === "round" && me.solved) || !current) return;
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
  setInterval(tick, 250); // 倒计时按本地时钟走，不必每秒打一次接口
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) refresh();
  });
})();