"use strict";

// 选手页和管理页共用的展示规则。
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
};
