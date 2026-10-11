"use strict";

// 选手页与管理页共用的展示规则。
window.WordleUI = {
  esc: value => String(value).replace(/[&<>"]/g, ch => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;",
  }[ch])),
  fmtMs: ms => {
    if (!ms) return "0 秒";
    if (ms < 1000) return ms + " 毫秒";
    const seconds = ms / 1000;
    if (seconds < 60) return seconds.toFixed(1) + " 秒";
    const rounded = Math.round(seconds);
    return Math.floor(rounded / 60) + " 分 " + (rounded % 60) + " 秒";
  },
  // 限时赛倒计时：向上取整到秒，m:ss。负数按 0 处理，到点后由状态轮询切到已结束。
  fmtLeft: ms => {
    const total = Math.max(0, Math.ceil(ms / 1000));
    return Math.floor(total / 60) + ":" + String(total % 60).padStart(2, "0");
  },
  // 玩法名，页面标题与徽标共用。
  modeName: mode => (mode === "timed" ? "限时赛" : "轮次赛"),
};