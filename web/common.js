"use strict";

// 选手页与管理页共用的展示规则。
(() => {
  const esc = value => String(value).replace(/[&<>"]/g, ch => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;",
  }[ch]));
  const fmtMs = ms => {
    if (!ms) return "0 秒";
    if (ms < 1000) return ms + " 毫秒";
    const seconds = ms / 1000;
    if (seconds < 60) return seconds.toFixed(1) + " 秒";
    const rounded = Math.round(seconds);
    return Math.floor(rounded / 60) + " 分 " + (rounded % 60) + " 秒";
  };

  window.WordleUI = {
    fmtMs,
    // 倒计时向上取整到秒，m:ss；负数按 0 处理。
    fmtLeft: ms => {
      const total = Math.max(0, Math.ceil(ms / 1000));
      return Math.floor(total / 60) + ":" + String(total % 60).padStart(2, "0");
    },
    modeName: mode => (mode === "timed" ? "限时赛" : "轮次赛"),
    // 页面各自处理标题和高亮，只共享榜单列与空状态。
    boardHead: mode => mode === "timed"
      ? "<tr><th>名次</th><th>昵称</th><th>答对题数</th><th>累计步数</th></tr>"
      : "<tr><th>名次</th><th>昵称</th><th>步数</th><th>用时</th></tr>",
    boardCells: (entry, mode) =>
      `<td>${entry.rank}</td><td>${esc(entry.displayName)}</td>` + (mode === "timed"
        ? `<td><b>${entry.solvedCount}</b></td><td>${entry.attempts}</td>`
        : `<td>${entry.attempts}</td><td>${fmtMs(entry.elapsedMs)}</td>`),
    boardEmpty: mode =>
      `<tr><td colspan="4" class="muted">${mode === "timed" ? "还没有人答对" : "还没有人解出"}</td></tr>`,
  };
})();
