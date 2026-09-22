/* ============================================================
 * 星塔旅人 活动日历 —— 甘特图业务逻辑（从单文件 template.html 原样迁出）
 * 时间解析 / 版本窗口 / 分组合并 / 泳道装箱 / 时间轴定位 / 16:9 出图
 * 全部保持与机器人单文件模板同一套口径，本文件只负责"算与画"，
 * 视觉皮肤在 css/style.css；数据由 Go 注入或 sample-data.js 兜底。
 * ============================================================ */
(function () {
  'use strict';
  const D = (typeof DATA !== 'undefined') ? DATA : window.DATA;
  if (!D) { console.error('缺少 DATA：既没有 Go 注入，也没有样本数据'); return; }

  const NOW = D.now;
  const RECS = D.records;
  const VERSIONS = D.versions;

  // 常驻玩法判据：≥2 期且期间排满（占满首末跨度 3/4 以上），名单由 Go 的 detectPermanent 下发
  const MONTHLY = {};
  (D.monthly || []).forEach(k => MONTHLY[k] = '常驻玩法：≥2 期，且各期排满整段日子（占首末跨度 3/4 以上）');

  const LBCLS = { '招募时间': 't-pool', '活动时间': 't-event', '开放时间': 't-mode', '测试信息': 't-test' };
  const TRACK_ICONS = {
    pool: '<svg class="ico-track ico-pool" viewBox="0 0 16 16" fill="currentColor"><path d="M8 1c.3 2.7 2.3 4.7 5 5-2.7.3-4.7 2.3-5 5-.3-2.7-2.3-4.7-5-5 2.7-.3 4.7-2.3 5-5z"/><circle cx="13.2" cy="2.8" r="1.3"/><circle cx="2.8" cy="13.2" r="0.9"/></svg>',
    ver: '<svg class="ico-track ico-ver" viewBox="0 0 16 16" fill="currentColor"><path fill-rule="evenodd" d="M2.2 1.5a.8.8 0 0 1 .8.8v.5h10.2c.6 0 .9.7.5 1.1L11.5 6.3l2.4 2.7c.4.4.1 1.1-.5 1.1H3v3.6a.8.8 0 0 1-1.6 0V2.3a.8.8 0 0 1 .8-.8zm4.9 3.5l.5 1.2 1.3.2-.9.9.2 1.3-1.1-.6-1.1.6.2-1.3-.9-.9 1.3-.2z"/></svg>',
    mon: '<svg class="ico-track ico-mon" viewBox="0 0 16 16"><path d="M13.8 6.5A6 6 0 0 0 3 4.2L2 2.5v4h4L4.3 4.9a4.5 4.5 0 0 1 8 1.6z" fill="currentColor"/><path d="M2.2 9.5a6 6 0 0 0 10.8 2.3l1 1.7v-4h-4l1.7 1.6a4.5 4.5 0 0 1-8-1.6z" fill="currentColor"/><polygon points="8,6 8.7,7.3 10,8 8.7,8.7 8,10 7.3,8.7 6,8 7.3,7.3" fill="currentColor"/></svg>'
  };
  const TRACKS = [
    { k: 'pool', name: '限定卡池' },
    { k: 'ver', name: '版本活动' },
    { k: 'mon', name: '周期玩法' },
  ];
  const DAY = 86400000, H8 = 8 * 3600e3, RESET = 4 * 3600e3;
  // 维护后开闸偏移由 Go 下发，机器人与网页共用一份，不在这里写死
  const OPEN = D.openOffsetMs;
  const ROW_H = 52, ROW_GAP = 5, PITCH = ROW_H + ROW_GAP;
  const PPD = 54, MIN_PPD = 50;
  const T = s => new Date(s.replace(' ', 'T') + ':00+08:00').getTime();
  const now = T(NOW);
  const day0 = ms => { const d = new Date(ms + H8); return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()) - H8; };
  const dow = ms => new Date(ms + H8).getUTCDay();
  const p2 = n => String(n).padStart(2, '0');
  const fMD = ms => { const d = new Date(ms + H8); return p2(d.getUTCMonth() + 1) + '/' + p2(d.getUTCDate()); };
  const CN = ['一', '二', '三', '四', '五', '六', '七', '八', '九', '十'], DK = ['日', '一', '二', '三', '四', '五', '六'];
  const tint = r => '#' + (r.tint || 'eef3fb');

  // 状态矢量图标（对齐二次元游戏手账视觉规范，替代纯汉字角标）
  const CLOCK = '<svg class="ico-clock" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="currentColor"/><path d="M12 7v5l3.2 2" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  // 未开始 / 未开启：游戏小锁头（圆底 + 锁梁与锁体钥匙孔）
  const ICO_UPCOMING = '<svg class="ico-state ico-upcoming" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9.5" fill="currentColor"/><rect x="8" y="11" width="8" height="6" rx="1.5" fill="#fff"/><path d="M9.5 11V8.5a2.5 2.5 0 0 1 5 0V11" fill="none" stroke="#fff" stroke-width="1.8" stroke-linecap="round"/><circle cx="12" cy="14" r="0.9" fill="currentColor"/></svg>';
  // 已结束：手账勾选章（圆底 + 清晰白色勾选完成标）
  const ICO_PAST = '<svg class="ico-state ico-past" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9.5" fill="currentColor"/><path d="M7.8 12.2l2.8 2.8 5.6-5.6" fill="none" stroke="#fff" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  // 领奖中：奖励礼盒（圆底 + 白色礼品盒与蝴蝶结）
  const ICO_CLAIM = '<svg class="ico-state ico-claim" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9.5" fill="currentColor"/><rect x="7.5" y="11" width="9" height="6.5" rx="1.2" fill="#fff"/><rect x="6.5" y="8.5" width="11" height="2.5" rx="1.2" fill="#fff"/><path d="M12 8.5v9" stroke="currentColor" stroke-width="1.4"/><path d="M10 8.5c-.8-1.2 0-2 1-1.5 1 .5 1 1.5 1 1.5s0-1 1-1.5c1-.5 1.8.3 1 1.5" fill="none" stroke="#fff" stroke-width="1.3" stroke-linecap="round"/></svg>';

  function esc(s) {
    if (s == null) return '';
    return String(s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  const opened = i => now >= T(VERSIONS[i].act0) + OPEN;
  function curIndex() {
    for (let i = VERSIONS.length - 1; i >= 0; i--) if (opened(i)) return i;
    return 0;
  }
  const CUR = curIndex();
  const state = { vi: CUR, day: 'nat', merge: true, drop: false, slot: false };

  // 出图模式 #x[键]：去导航/开关，锁 16:9，画指定版本
  const EX = /^#x(\d{12})?$/.test(location.hash);
  if (EX) {
    document.body.classList.add('x');
    const k = /^#x(\d{12})$/.exec(location.hash);
    if (k) {
      const i = VERSIONS.findIndex(v => v.key === k[1]);
      if (i >= 0) state.vi = i; else console.warn('未知版本键', k[1]);
    }
  }

  function frame() {
    const v = VERSIONS[state.vi];
    const t0 = T(v.act0);
    const g0 = state.day === 'game' ? day0(t0) + RESET : day0(t0);
    const n = Math.ceil((day0(T(v.redeem)) + DAY - g0) / (7 * DAY)) * 7;
    return { v, g0, g1: g0 + n * DAY, w0: t0 + OPEN, n };
  }
  const gday = ms => day0(ms - RESET);
  const dayOf = ms => state.day === 'game' ? gday(ms) : day0(ms);
  const pct = (ms, f) => (Math.max(f.g0, Math.min(f.g1, ms)) - f.g0) / (f.g1 - f.g0) * 100;

  const DAYMODES = [
    { k: 'nat', n: '自然日', tip: '日格按 00:00 分列，结束日期标签取自然日。条带位置不受影响。' },
    { k: 'game', n: '游戏日 04:00', tip: '日格按 04:00 分列（跟游戏内刷新一致），结束日期取最后一个完整游戏日。' },
  ];

  function vTag(i) {
    return i === CUR && opened(i) ? '当前版本' : i < CUR ? '上一版本' : '下一版本';
  }

  function groups(f) {
    let rs = RECS.map(r => {
      const s0 = T(r.s) + (r.st === 'fuzzy' ? OPEN : 0);
      const e0 = T(r.e);
      return { ...r, s0, e0, cE: r.ce ? T(r.ce) : 0 };
    })
      .filter(r => r.e0 > f.w0 && r.s0 < f.g1)
      .filter(r => state.drop || !r.drop);
    if (!state.merge) rs = rs.map(r => ({ items: [r] }));
    else {
      const g = new Map();
      for (const r of rs) {
        const k = (r.drop || r.lb !== '招募时间') ? 'solo:' + r.id : [r.s0, r.e0, r.lb].join('|');
        if (!g.has(k)) g.set(k, []);
        g.get(k).push(r);
      }
      rs = [...g.values()].map(items => ({ items }));
    }
    for (const g of rs) {
      const it = g.items;
      g.lb = it[0].lb;
      g.s0 = Math.min(...it.map(r => r.s0));
      g.e0 = Math.max(...it.map(r => r.e0));
      g.monthly = it.some(r => MONTHLY[r.n]);
      g.track = g.lb === '招募时间' ? 'pool' : g.monthly ? 'mon' : 'ver';
      g.x1 = g.track === 'mon' ? g.e0 : Math.max(g.e0, ...it.map(r => r.cE || 0));
      g.oL = g.s0 < f.g0;
      g.oR = g.x1 > f.g1;
    }
    return rs;
  }

  // 按真实时刻装箱泳道
  function pack(gs) {
    const free = [];
    gs.sort((a, b) => a.s0 - b.s0 || a.x1 - b.x1);
    for (const g of gs) {
      const i = free.findIndex(x => x <= g.s0);
      if (i < 0) { free.push(g.x1); g.lane = free.length; }
      else { free[i] = g.x1; g.lane = i + 1; }
    }
    return free.length;
  }

  function sizeWin(f) {
    const win = document.querySelector('.win');
    const clamp = w => Math.max(w, f.n * MIN_PPD + 96);
    if (!EX) { win.style.width = (f.n * PPD + 96) + 'px'; return; }
    let w = f.n * PPD + 96;
    for (let i = 0; i < 3; i++) {
      w = clamp(w);
      win.style.width = w + 'px';
      w = clamp(win.offsetHeight * 16 / 9);
    }
    win.style.width = w + 'px';
    document.title = Math.round(parseFloat(win.style.width)) + 'x' + win.offsetHeight;
  }

  function render() {
    const f = frame(), cols = `repeat(${f.n},1fr)`;
    const all = groups(f);

    document.getElementById('vname').innerHTML = `<span class="tag-lead">★ VER</span><span class="tag-txt">${esc(f.v.name)}</span>`;
    document.getElementById('vers').innerHTML = VERSIONS.map((v, i) =>
      `<button data-i="${i}"${i === state.vi ? ' class="on"' : ''} title="活动一览 ${esc(v.src)}｜${esc(v.name)}">${vTag(i)}</button>`).join('');
    document.querySelectorAll('#vers button').forEach(b => b.onclick = () => { state.vi = +b.dataset.i; render(); });
    document.getElementById('dayseg').innerHTML = DAYMODES.map(d =>
      `<button data-k="${d.k}"${d.k === state.day ? ' class="on"' : ''} title="${d.tip}">${d.n}</button>`).join('');
    document.querySelectorAll('#dayseg button').forEach(b => b.onclick = () => { state.day = b.dataset.k; render(); });

    const tAnchor = state.day === 'game' ? gday(now) + RESET : day0(now);
    let wk = '', drow = '', c = 1, wi = 0;
    while (c <= f.n) {
      const span = Math.min(7, f.n - c + 1), s = f.g0 + (c - 1) * DAY, on = now >= s && now < s + span * DAY;
      wk += `<div class="wk${on ? ' on' : ''}" style="grid-column:span ${span}"><b>第${CN[wi]}周</b><i>${fMD(s)}-${fMD(s + (span - 1) * DAY)}</i></div>`;
      for (let k = 0; k < span; k++) {
        const d = dow(s + k * DAY), isT = s + k * DAY === tAnchor;
        drow += `<span class="${isT ? 't' : d === 0 ? 'a' : d === 6 ? 'b' : ''}">${DK[d]}</span>`;
      }
      c += span; wi++;
    }
    const wrow = document.getElementById('weeks'); wrow.style.gridTemplateColumns = cols; wrow.innerHTML = wk;
    const dday = document.getElementById('days'); dday.style.gridTemplateColumns = cols; dday.innerHTML = drow;

    const cw = 100 / f.n;
    const inWin = now >= f.g0 && now < f.g1;
    const cells = () => {
      let s = '';
      for (let i = 0; i < f.n; i++) {
        const d = f.g0 + i * DAY;
        s += `<div class="cell${i % 7 === 0 ? ' wk0' : ''}" style="left:${i * cw}%;width:${cw}%"></div>`;
      }
      return s;
    };
    // 红墨水手绘虚线：出图时刻贯穿三轨道，顶部手绘红圈由 CSS 画
    const nowLine = () => inWin
      ? `<div class="nowline" style="left:calc(16px + (100% - 32px) * ${(pct(now, f) / 100).toFixed(6)})"></div>` : '';

    document.getElementById('plot').innerHTML = TRACKS.map(t => {
      const gs = all.filter(g => g.track === t.k);
      const nL = gs.length ? Math.max(1, pack(gs)) : 0;
      const body = gs.length
        ? `<div class="rows" style="height:${nL * PITCH - ROW_GAP + 8}px">${cells()}${gs.map(g => band(g, f)).join('')}</div>`
        : `<div class="rows empty">无${t.name}</div>`;
      return `<div class="grp ${t.k}"><div class="gh"><span class="gt">${TRACK_ICONS[t.k] || ''}${t.name}</span>
        <span class="gs">${gs.length} 条</span><span class="gl"></span></div>${body}</div>`;
    }).join('') + nowLine();

    function band(g, f) {
      let l = pct(g.s0, f);
      const b = pct(g.e0, f), r = pct(g.x1, f);
      const hasTail = r > b + 1e-9;
      const minW = 0.8 * cw;
      if (!hasTail && b - l < minW) l = Math.max(0, b - minW);
      const past = g.e0 <= now;
      const upcoming = g.s0 > now;                 // 还没开
      const claiming = past && now < g.x1;        // 玩法结束、仍在领奖/兑换尾段
      const cls = ['band', hasTail || g.oR ? 'oR' : '', g.oL ? 'oL' : '',
        upcoming ? 'upcoming' : past ? 'past' : 'live',
        (g.e0 - Math.max(g.s0, f.g0)) / DAY <= 4 ? 'narrow' : '',
        g.items[0].drop ? 'drop' : ''].filter(Boolean).join(' ');
      // 结束/状态胶囊：
      // 主条带显示活动的到期时间 g.e0（进行中展示时钟，未开始显示锁头，已结束显示勾选章）
      const msLeft = g.e0 - now;
      const urgent = !past && !upcoming && msLeft > 0 && msLeft <= 2 * DAY;
      const statusIco = upcoming ? ICO_UPCOMING
        : past ? ICO_PAST
        : CLOCK;
      const endsStatusCls = upcoming ? 'upcoming' : past ? 'past' : 'live';
      const ends = `<span class="ends ${endsStatusCls}${urgent ? ' hot' : ''}" title="${upcoming ? '未开启' : past ? '已结束' : '进行中'}">${statusIco}${fMD(dayOf(g.e0))}</span>`;
      const segs = g.items.map(x => {
        const art = x.poster || '';
        const pos = `object-position:${(x.ax ?? 25)}% ${(x.ay ?? 45)}%`;
        return `<span class="seg-i">
          <span class="ava">
            ${art ? `<img src="${art}" style="${pos}">` : '<i class="noart"></i>'}
            ${state.slot && !x.drop ? '<span class="rar">?</span>' : ''}</span>
          <span class="txt"><span class="nm">${x.st === 'fuzzy' ? '≈' : ''}${esc(x.n)}</span></span></span>`;
      }).join('');
      const at = `top:${(g.lane - 1) * PITCH}px;height:${ROW_H}px`;

      // 领奖/兑换尾段：有尾段时在尾段展示最终领奖截止日期与礼盒图标
      let tail = '';
      if (hasTail) {
        const claimPast = g.x1 <= now;
        const claimLeft = g.x1 - now;
        const claimUrgent = !claimPast && claimLeft > 0 && claimLeft <= 2 * DAY;
        const claimIco = claimPast ? ICO_PAST : ICO_CLAIM;
        const claimCls = claimPast ? 'past' : 'claim';
        const tailEnds = `<span class="ends ${claimCls}${claimUrgent ? ' hot' : ''}" title="领奖/兑换截止：${fMD(dayOf(g.x1))}">${claimIco}${fMD(dayOf(g.x1))}</span>`;
        tail = `<span class="tail${g.oR ? ' oR' : ''}" style="left:${b}%;width:${r - b}%;${at}">${tailEnds}</span>`;
      }
      const cnt = g.items.length > 1
        ? `<span class="cnt"><svg class="ico-cnt" viewBox="0 0 16 16"><rect x="2" y="3.5" width="8.5" height="9.5" rx="1.5" fill="none" stroke="currentColor" stroke-width="1.4"/><rect x="5.5" y="2" width="8.5" height="9.5" rx="1.5" fill="none" stroke="currentColor" stroke-width="1.4"/></svg><span>${g.items.length}项并排</span></span>`
        : '';
      const tags = state.slot && !g.items[0].drop ? `<span class="tags"><i class="tag"></i><i class="tag"></i></span>` : '';
      const tip = g.items.map(r => `ID ${r.id}｜${r.lb}｜${r.st === 'fuzzy' ? 'fuzzy_start（原文"维护结束后"；库内存 00:00 下界，图上按开闸估计摆放）' : 'ok'}
${r.s} → ${r.e}${r.ce ? `\n领奖/兑换期 ${r.cs} → ${r.ce}` : ''}
原文「${r.f}」${r.drop ? '\n已过滤：' + r.drop : ''}${r.src === 'summary' ? '\n出自汇总公告 ' + r.ref + ' 正文切分（该活动尚无独立公告，故无海报）' : r.src === 'version' ? '\n出自「活动一览」 ' + r.ref + '（版本主活动，封面即版本主视觉）' : '\n独立公告 ' + r.ref}${g.monthly ? '\n周期玩法：' + MONTHLY[r.n] : ''}`).join('\n——\n');
      const wrapStatus = upcoming ? 'upcoming' : past ? 'past' : 'live';
      return `<div class="band-wrap ${wrapStatus}" style="left:${l}%;width:${(hasTail ? b : r) - l}%;${at}">
        <div class="${cls}" style="--tint:${tint(g.items[0])}" title="${tip.replace(/"/g, '&quot;')}">
          ${g.items[0].drop ? '<span class="stamp">已过滤</span>' : ''}
          <span class="play">${segs}${cnt}${tags}${ends}</span>
        </div>
      </div>${tail}`;
    }

    const nRec = all.reduce((s, g) => s + g.items.length, 0);
    const live = all.filter(g => g.e0 > now && g.s0 <= now).length;
    const mt = all.filter(g => g.items.some(r => r.src === 'summary')).length;
    document.getElementById('foot').innerHTML = `
      <div class="frow">
        <span class="h">图例</span>
        <span class="key">${ICO_PAST}已结束</span>
        <span class="key">${ICO_UPCOMING}未开始</span>
        <span class="key">${ICO_CLAIM}领奖/兑换期</span>
        <span class="key">${CLOCK}结束时间（≤2天转红）</span>
        <span class="key"><span class="sw hatch"></span>领奖尾段</span>
        <span class="key"><span class="sw open"></span>两端不封口＝跨窗口延续</span>
        <span class="key"><span class="sw" style="background:repeating-linear-gradient(45deg,#e2e8f3 0 4px,#f4f7fc 4px 8px)"></span>无海报</span>
        <span class="key"><span class="sw" style="background:#fff;border:1.2px dashed #7894b6"></span>N项并排＝一条带并排多项玩法</span>
        <span class="key"><b>≈</b>起点是估的（按 ${OPEN / 3600e3}:00 开闸摆放）</span>
        ${inWin ? `<span class="key"><span class="sw" style="background:#e85a6a;border-radius:1px;width:3px"></span>红虚线＝出图时刻</span>` : ''}
      </div>
      <div class="frow">
        <span>条带=<b>真实时刻</b>（10:59 收就停在 10:59，不吸附日格）· 日格=<b>${state.day === 'game' ? '游戏日 04:00' : '自然日 00:00'}</b>（只改格子边界与日期标签取整）· 尾段取活动公告的领取/兑换行 · 同名同窗才并带，复玩两期各占一条带</span>
        <span><b>${f.n}</b> 天 / <b>${all.length}</b> 带 · 合并掉 ${nRec - all.length} · 进行中 <b>${live}</b>${mt ? ' · 维护公告补的 <b>' + mt + '</b> 带' : ''}</span>
        <span class="cr">版本窗口取自活动一览 ${f.v.src} · 数据 data/xingta-live.db</span>
      </div>`;

    ['tMerge', 'tDrop', 'tSlot'].forEach(id =>
      document.getElementById(id).classList.toggle('on', state[id.slice(1).toLowerCase()]));
    sizeWin(f);
  }

  document.getElementById('tMerge').onclick = () => { state.merge = !state.merge; render(); };
  document.getElementById('tDrop').onclick = () => { state.drop = !state.drop; render(); };
  document.getElementById('tSlot').onclick = () => { state.slot = !state.slot; render(); };
  render();
})();
