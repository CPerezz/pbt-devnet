// Slot River: the migration devnet as a block DAG on a time axis, one lane
// per lineage segment, node chips riding their heads, and a per-node strip
// beneath. Pure render(snapshot, prev): the server has already derived
// lineage, depth and agreement; this file only draws.

const NS = 'http://www.w3.org/2000/svg';
const LANE_H = 38, BLOCK_R = 6, STRIP_ROW = 26, RIBBON_H = 22, AGREE_H = 8, AXIS_H = 18, TOP_PAD = 26;
const FORMAT = { mpt: '#4c8dff', pbt: '#ff9a3c' };
const OTHER = { mpt: 'pbt', pbt: 'mpt' };
const PHASE = { following: '#9fd6a3', synced: '#2fa84f', parked: '#8d8d8d', window: '#ffc27a', done: '#ff8a1f', stalled: '#e0433d', opaque: '#5a5a5a', unknown: '#c8c8c8' };
const AGREE = { pending: '#8a8a8a', partial: '#8a8a8a', all: '#2fa84f', single: '#8a8a8a', split: '#e0433d', gone: 'none', none: 'none' };
const SEGMENT_HUES = ['#2b2b2b', '#b04ad6', '#1eaaa0', '#d67f1e', '#d6478f', '#5e73d6'];
const CLASS = { deep: '#b04ad6', short: '#1eaaa0', straddle: '#e0433d', window: '#d6478f', scenario: '#5e73d6' };
const CLASS_DOC = {
  deep: 'Deep partition: the victim is cut off (EL+CL p2p) long enough to mint its own branch well below the majority. On heal it must rewind ≥10 blocks and its PBT follower must re-anchor on the winning chain.',
  short: 'Short partition: a light node is isolated briefly, so the branch is shallow and the rewind small. Checks the follower survives a shallow reorg.',
  straddle: 'Straddle: a partition spanning I*. Each side crosses the activation on its own block, so there are two I* candidates. On heal the victim rewinds below I* (back to MPT-primary blocks, H*) and re-crosses on the winner\'s I*.',
  window: 'Window partition: isolation inside the open migration window (after I*, while the reverse MPT direction still runs). Checks the shadow MPT root survives a reorg.',
  scenario: 'Post-fork scenario: the chaos driver writes state on a doomed two-node island, heals it, and checks that state is gone from every client afterwards.',
};
const CHIP_R = 11;

// Client marks, each the project's own: the ethereum diamond for geth, and the
// symbol every other client ships. They are drawn at ~16px, where interior
// detail is lost but the silhouettes stay distinct, and every chip keeps its
// participant number in a corner badge.
// White is the house ink, but it disappears on the pale fills (window, unknown),
// so the mark falls back to dark whenever white's contrast against the disc drops
// too low. Computed rather than listed: a new phase colour cannot silently
// produce an illegible chip.
const luminance = hex => {
  const c = i => { const v = parseInt(hex.slice(i, i + 2), 16) / 255; return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
  return 0.2126 * c(1) + 0.7152 * c(3) + 0.0722 * c(5);
};
const glyphInk = phase => (1.05 / (luminance(PHASE[phase] || PHASE.unknown) + 0.05) < 2 ? '#1a1a1a' : '#fff');

const LOGOS = {
  geth: { box: '0 0 1920 1920', body: '<polygon points="959.8,80.7 420.1,976.3 959.8,1295.4 1499.6,976.3"/><polygon points="420.1,1078.7 959.8,1839.3 1499.6,1078.7 959.8,1397.6"/>' },
  besu: { box: '0.93 0.07 68.65 95.22', body: '<path d="M69.58,39.36 L69.58,39.13 C69.58,36.65 67.57,34.63 65.08,34.63 L64.85,34.63 C63.87,34.63 62.97,34.94 62.24,35.47 L39.87,22.56 C39.88,22.45 39.89,22.33 39.89,22.22 L39.89,21.99 C39.89,19.51 37.88,17.49 35.39,17.49 L35.16,17.49 C32.68,17.49 30.66,19.5 30.66,21.99 L30.66,22.22 C30.66,22.33 30.67,22.45 30.68,22.56 L8.31,35.47 C8.05,35.29 7.78,35.13 7.48,35 L7.48,8.93 C9.09,8.2 10.21,6.59 10.21,4.71 C10.21,2.15 8.13,0.07 5.57,0.07 C3.01,0.07 0.93,2.15 0.93,4.71 C0.93,6.59 2.05,8.21 3.66,8.93 L3.66,35 C2.07,35.69 0.95,37.28 0.95,39.13 L0.95,39.36 C0.95,41.21 2.07,42.79 3.66,43.49 L3.66,69.29 C2.07,69.98 0.95,71.57 0.95,73.42 L0.95,73.65 C0.95,76.13 2.96,78.15 5.45,78.15 L5.68,78.15 C6.65,78.15 7.55,77.84 8.29,77.31 L30.66,90.22 C30.65,90.33 30.64,90.44 30.64,90.56 L30.64,90.79 C30.64,93.27 32.65,95.29 35.14,95.29 L35.37,95.29 C37.85,95.29 39.87,93.28 39.87,90.79 L39.87,90.56 C39.87,90.44 39.86,90.33 39.85,90.22 L62.23,77.3 C62.97,77.83 63.87,78.14 64.84,78.14 L65.07,78.14 C67.55,78.14 69.57,76.13 69.57,73.64 L69.57,73.41 C69.57,71.56 68.45,69.98 66.86,69.28 L66.86,43.48 C68.46,42.79 69.58,41.21 69.58,39.36 Z M63.06,68.57 C60.93,65.08 57.31,61.29 52.88,57.61 C51.86,58.42 50.82,59.22 49.77,60 C55.44,64.65 59.09,68.96 60.58,72 C60.43,72.44 60.35,72.92 60.35,73.41 L60.35,73.64 C60.35,73.75 60.36,73.87 60.37,73.98 L38,86.89 C37.43,86.48 36.75,86.2 36.02,86.1 C33.19,81.42 30.48,70.81 30.48,56.38 C30.48,54.14 30.55,52 30.67,49.96 C29.39,49.32 28.16,48.73 26.97,48.2 C26.77,50.89 26.67,53.64 26.67,56.38 C26.67,67.99 28.45,79.69 31.9,86.52 L11.3,74.62 C15.15,74.62 20.16,73.43 25.75,71.25 C25.57,69.97 25.42,68.66 25.3,67.34 C17.93,70.33 12.26,71.25 9.25,70.64 C8.79,70.05 8.19,69.58 7.5,69.28 L7.5,43.48 C8.2,43.18 8.8,42.7 9.26,42.12 C13.87,41.2 24.68,43.84 38.26,51.96 C40.07,53.04 41.78,54.14 43.39,55.23 C44.54,54.43 45.62,53.63 46.64,52.85 C44.57,51.4 42.41,50.01 40.21,48.69 C29.06,42.03 18.27,38.15 11.31,38.14 L31.92,26.24 C30.11,29.83 28.76,34.76 27.89,40.31 C29.07,40.8 30.28,41.33 31.51,41.9 C32.55,34.84 34.27,29.59 36.04,26.66 C36.77,26.56 37.45,26.28 38.02,25.87 L60.39,38.78 C60.38,38.89 60.37,39 60.37,39.12 L60.37,39.35 C60.37,39.85 60.45,40.32 60.6,40.77 C58.26,45.54 50.62,53.41 38.26,60.8 C36.34,61.95 34.47,62.99 32.66,63.92 C32.76,65.34 32.89,66.7 33.03,68 C35.38,66.83 37.78,65.51 40.2,64.07 C50.05,58.18 59.11,50.66 63.07,44.2 L63.07,68.57 L63.06,68.57 Z" stroke="currentColor" stroke-width="3.5" stroke-linejoin="round"/>' },
  erigon: { box: '0 0 1024 1024', body: '<polygon points="512 576 288 480 416 617.14 288 672 288 960 512 720 736 960 736 672 608 617.14 736 480 512 576"/><polygon points="736 416 512 64 288 416 512 512 736 416"/>' },
  nethermind: { box: '0 0 160 81', body: '<path d="M152.844 27.4198L131.751 33.9283C129.055 26.9958 122.21 22.1786 114.346 22.4355C110.704 22.5545 107.354 23.7533 104.579 25.6996L88.3179 10.4044C95.071 4.42228 103.877 0.66362 113.6 0.345997C129.415 -0.170665 143.39 8.55138 150.331 21.641C150.344 21.6696 150.362 21.698 150.375 21.7224C150.689 22.3211 150.994 22.9241 151.279 23.5361C151.863 24.7968 152.382 26.0805 152.84 27.4158L152.844 27.4198Z"/><path d="M67.393 69.4134C61.688 74.7241 54.415 78.3977 46.289 79.6477C45.4874 79.7755 44.6715 79.8708 43.8546 79.9496C43.3599 80.0021 42.8641 80.0381 42.3638 80.0661C42 80.0864 41.6315 80.0987 41.2716 80.1146C41.0313 80.1198 40.7866 80.121 40.5461 80.122C40.3348 80.1255 40.1278 80.1329 39.9158 80.1241C39.783 80.1232 39.6546 80.1262 39.5216 80.1212C39.3059 80.1208 39.0943 80.1202 38.8821 80.1072C38.3379 80.092 37.7971 80.0641 37.2518 80.0283C36.9148 80.0098 36.581 79.9745 36.2475 79.9434C35.9514 79.9143 35.6594 79.885 35.363 79.8518C35.2335 79.8342 35.1124 79.8202 34.9828 79.8026C34.7405 79.7705 34.494 79.7387 34.2555 79.7023C19.7107 77.5669 7.54583 67.5493 2.53369 53.9312L23.5467 46.8945C26.3691 53.8352 33.3644 58.5525 41.27 58.1108C44.8755 57.9094 48.1693 56.6556 50.8871 54.6712L67.385 69.418L67.393 69.4134Z"/><path d="M22.2614 41.1133C22.0143 36.6906 23.3843 32.555 25.8555 29.2741L9.26597 14.4453C7.48608 16.5888 5.92838 18.9231 4.61514 21.4013L3.96542 22.6897C2.57121 25.5746 1.51978 28.6559 0.848569 31.8611C0.144781 35.2257 -0.137448 38.7119 0.06298 42.2996C0.234544 45.3706 0.745369 48.3522 1.58464 51.1994L22.6967 44.153C22.4715 43.1664 22.3194 42.1507 22.2612 41.1091L22.2614 41.1133Z"/><path d="M39.2629 22.1012C40.309 22.0428 41.3356 22.0808 42.3415 22.1946L46.8779 0.567432C43.9549 0.0673437 40.9338 -0.108012 37.8452 0.064548C34.2686 0.264366 30.8219 0.93374 27.567 1.99044C24.443 3.01495 21.4936 4.41944 18.7686 6.12651L17.6005 6.89663C15.271 8.4821 13.1237 10.2854 11.1919 12.3089L27.764 27.122C30.7671 24.2302 34.7769 22.356 39.2673 22.1051L39.2629 22.1012Z"/><path d="M54.744 29.3463L56.1314 30.6453L70.7842 14.295C69.9186 13.2778 69.0008 12.2925 68.0399 11.3511L66.8106 10.2091C66.8106 10.2091 66.8099 10.1967 66.7975 10.1974C64.3571 8.01601 61.6301 6.12428 58.7072 4.58763L57.4903 3.96321C55.035 2.77774 52.4022 1.83023 49.6845 1.14453L45.1553 22.7506C49.0194 23.8366 52.3817 26.1862 54.7401 29.3507L54.744 29.3463Z"/><path d="M100.479 51.2404L98.6555 49.4609L83.6406 65.4714C84.4783 66.5085 85.3606 67.5152 86.299 68.462L87.5019 69.6446C87.5019 69.6446 87.5143 69.6442 87.5147 69.6566C89.9205 71.8934 92.5865 73.8483 95.473 75.4522L96.6746 76.088C99.1189 77.346 101.712 78.3548 104.413 79.0991L109.447 57.6367C105.833 56.4832 102.706 54.2203 100.479 51.2363L100.479 51.2404Z"/><path d="M127.097 53.7737C124.035 56.5701 119.999 58.3339 115.529 58.4799C114.411 58.5164 113.315 58.4446 112.249 58.2847L107.215 79.7388C110.126 80.3065 113.142 80.5517 116.23 80.4508C119.81 80.3339 123.271 79.7445 126.55 78.7634C129.697 77.8115 132.678 76.4757 135.442 74.8322L136.627 74.0893C138.993 72.5582 141.181 70.805 143.159 68.8268L127.097 53.7695L127.097 53.7737Z"/><path d="M153.668 30.1722L132.574 36.6725C132.795 37.709 132.933 38.7773 132.969 39.8696C133.114 44.3216 131.629 48.4458 129.054 51.6778L145.133 66.7346C146.962 64.6329 148.573 62.3353 149.944 59.8882L150.623 58.6151C152.084 55.7633 153.223 52.7066 153.967 49.5012C154.737 46.1667 155.096 42.6881 154.978 39.0967C154.877 36.006 154.436 33.0299 153.663 30.1641L153.668 30.1722Z"/>' },
};


const el = (tag, attrs = {}, parent) => {
  const n = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  if (parent) parent.appendChild(n);
  return n;
};
const short = (h) => (h && h.length > 12 ? h.slice(0, 8) + '…' + h.slice(-4) : h || '');
const segHue = (seg) => SEGMENT_HUES[Math.abs(seg.lane) % SEGMENT_HUES.length];
const clock = (slot, s) => { const t = slot * s.slot_seconds; return `${Math.floor(t / 60)}m${String(t % 60).padStart(2, '0')}s`; };
const esc = (t) => String(t).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

// Shadow-root classes of one block: majority first, then the rest.
function rootClasses(b, s) {
  const groups = {};
  for (const [n, r] of Object.entries(b.shadow_roots)) (groups[r] ||= []).push(+n);
  const classes = Object.entries(groups).map(([root, nodes]) => ({ root, nodes: nodes.sort((a, c) => a - c) })).sort((a, c) => c.nodes.length - a.nodes.length);
  const reported = new Set(Object.keys(b.shadow_roots).map(Number));
  const missing = s.nodes.map(n => n.id).filter(id => !reported.has(id));
  return { classes, missing };
}

export class River {
  constructor(root, panels) {
    this.root = root;
    this.panels = panels; // { tip, block, moves }
    this.view = { from: 0, to: 100, follow: true };
    this.pxPerSlot = 12;
    this.prev = null;
    this.pinned = null;
    this.svg = el('svg', { class: 'river' }, root);
    this.defs = el('defs', {}, this.svg);
    this.buildDefs();
    this.layers = {};
    for (const name of ['tint', 'bands', 'finality', 'edges', 'moves', 'blocks', 'cursors', 'chips', 'axis', 'badges']) this.layers[name] = el('g', { class: name }, this.svg);
    this.ribbon = el('svg', { class: 'ribbon' }, root); root.insertBefore(this.ribbon, this.svg);
    this.agree = el('svg', { class: 'agree' }, root); root.insertBefore(this.agree, this.svg);
    this.strip = el('svg', { class: 'strip' }, root);
    this.bindPanZoom();
  }

  buildDefs() {
    const hatch = (id, fill) => {
      const p = el('pattern', { id, width: 6, height: 6, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' }, this.defs);
      el('rect', { width: 2, height: 6, fill }, p);
    };
    hatch('hatch', 'rgba(0,0,0,0.18)'); hatch('hatch-red', 'rgba(224,67,61,0.55)');
    for (const [client, logo] of Object.entries(LOGOS)) {
      const sym = el('symbol', { id: 'logo-' + client, viewBox: logo.box, fill: 'currentColor' }, this.defs);
      sym.innerHTML = logo.body;
    }
    const marker = (id, color) => {
      const m = el('marker', { id, viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse' }, this.defs);
      el('path', { d: 'M0,0 L10,5 L0,10 z', fill: color }, m);
    };
    marker('arrow-red', '#e0433d'); marker('arrow-green', '#2fa84f');
  }

  x(slot) { return (slot - this.view.from) * this.pxPerSlot + 24; }
  laneY(lane) { return this.laneMid - lane * LANE_H; }

  render(s) {
    const prev = this.prev;
    const width = this.root.clientWidth || 1200;
    if (this.view.follow) {
      // keep ~90px of room right of "now" so the head chips never clip
      const span = this.view.to - this.view.from;
      this.view.to = s.now_slot + Math.max(3, Math.ceil(90 / ((width - 48) / span)));
      this.view.from = this.view.to - span;
    }
    this.pxPerSlot = (width - 48) / (this.view.to - this.view.from);
    const lanes = s.segments.map(x => x.lane);
    const minLane = Math.min(0, ...lanes), maxLane = Math.max(0, ...lanes);
    const riverH = (maxLane - minLane + 1) * LANE_H + TOP_PAD + 20 + AXIS_H;
    this.laneMid = TOP_PAD + maxLane * LANE_H + LANE_H / 2;
    this.svg.setAttribute('viewBox', `0 0 ${width} ${riverH}`); this.svg.setAttribute('height', riverH);
    this.ribbon.setAttribute('viewBox', `0 0 ${width} ${RIBBON_H + 14}`); this.ribbon.setAttribute('height', RIBBON_H + 14);
    this.agree.setAttribute('viewBox', `0 0 ${width} ${AGREE_H}`); this.agree.setAttribute('height', AGREE_H);
    const stripH = s.nodes.length * STRIP_ROW + 4;
    this.strip.setAttribute('viewBox', `0 0 ${width} ${stripH}`); this.strip.setAttribute('height', stripH);

    const inView = (slot) => slot >= this.view.from - 2 && slot <= this.view.to + 2;
    const segById = this.segById = Object.fromEntries(s.segments.map(x => [x.id, x]));
    const blockByHash = this.blockByHash = Object.fromEntries(s.blocks.map(b => [b.hash, b]));
    const lod = this.pxPerSlot >= 12 ? 0 : this.pxPerSlot >= 5 ? 1 : 2;
    this.width = width;
    this.s = s;

    this.drawRibbon(s, width);
    this.drawAgreeBar(s, segById, width);
    this.drawTint(s, riverH, width);
    this.drawBands(s, riverH);
    this.drawFinality(s, riverH);
    this.drawSegments(s, segById, blockByHash, inView, lod);
    this.drawMoves(s, segById, blockByHash);
    this.drawChips(s, prev, segById, blockByHash);
    this.drawAxis(s, riverH, width);
    this.drawStrip(s, segById, width, stripH);
    this.animateReorgs(s, prev);
    this.renderMoves(s, segById);
    this.refreshCard();
    this.refreshTip();
    this.prev = s;
  }

  // Schedule ribbon: planned ops on the same time axis, the active one saturated.
  drawRibbon(s, width) {
    const g = this.ribbon; g.replaceChildren();
    el('rect', { x: 0, y: 0, width, height: RIBBON_H, fill: '#f4f4f4' }, g);
    for (const op of s.schedule) {
      const end = op.end_slot || s.now_slot;
      if (end < this.view.from || op.start_slot > this.view.to) continue;
      const active = op.start_slot <= s.now_slot && s.now_slot < (op.end_slot || Infinity);
      const done = op.end_slot && op.end_slot <= s.now_slot;
      const x0 = Math.max(this.x(op.start_slot), 0), x1 = Math.min(this.x(end), width);
      el('rect', { x: x0, y: 2, width: Math.max(x1 - x0, 2), height: RIBBON_H - 4, rx: 3, fill: CLASS[op.class] || '#999', opacity: active ? 0.95 : done ? 0.3 : 0.55, class: 'op', 'data-tip': 'op:' + s.schedule.indexOf(op) }, g);
      if (x1 - x0 > 40) el('text', { x: x0 + 4, y: RIBBON_H - 7, class: 'ribbon-label', fill: '#fff', 'pointer-events': 'none' }, g).textContent = op.class === 'scenario' ? op.name : op.class;
    }
    el('line', { x1: this.x(s.fork_slot), x2: this.x(s.fork_slot), y1: 0, y2: RIBBON_H + 14, class: 'fork-rule' }, g);
    const nx = this.x(s.now_slot);
    el('polygon', { points: `${nx - 5},${RIBBON_H + 14} ${nx + 5},${RIBBON_H + 14} ${nx},${RIBBON_H + 6}`, fill: '#222' }, g);
    for (let slot = Math.max(0, Math.ceil(this.view.from / 15) * 15); slot <= this.view.to; slot += 15) {
      if (Math.abs(this.x(slot) - this.x(s.fork_slot)) < 18) continue;
      el('text', { x: this.x(slot), y: RIBBON_H + 11, class: 'axis-label', 'text-anchor': 'middle' }, g).textContent = slot;
    }
  }

  // What a scheduled op is, where it stands, and what it did to the chain.
  opCard(op, s) {
    const status = op.start_slot <= s.now_slot && s.now_slot < (op.end_slot || Infinity) ? 'active' : op.end_slot && op.end_slot <= s.now_slot ? 'done' : 'planned';
    const part = s.partitions.find(p => p.name === op.name);
    const heals = s.reorgs.filter(r => op.victims && op.victims.includes(r.node) && r.slot >= (op.end_slot || 0) && r.slot <= (op.end_slot || Infinity) + 12 && op.end_slot);
    const lines = [
      `<b>${esc(op.class)} · ${esc(op.name)}</b> <span class="muted">${status}</span>`,
      `<div class="doc">${esc(CLASS_DOC[op.class] || '')}</div>`,
      `planned: slots ${op.start_slot}–${op.end_slot || '…'} (${clock(op.start_slot, s)} → ${op.end_slot ? clock(op.end_slot, s) : '…'})` + (op.victims ? ` · victim${op.victims.length > 1 ? 's' : ''} node ${op.victims.join(', ')}` : ''),
    ];
    if (part) lines.push(`applied: slot ${part.applied_slot} → ${part.lifted_slot ? 'lifted at ' + part.lifted_slot : 'still open'}`);
    else if (status !== 'planned') lines.push('<span class="muted">no partition record from disruptoor</span>');
    for (const r of heals) lines.push(`<span class="bad">healed at slot ${r.slot}: node ${r.node} rewound ${r.depth} to #${r.ancestor_number}, +${r.added} on the winner${r.crosses_istar ? ', re-crossed I*' : ''}</span>`);
    if (status === 'done' && !heals.length) lines.push('<span class="muted">no rewind recorded (victim never diverged)</span>');
    return lines.join('<div></div>');
  }

  // Agreement bar: worst shadow-root state among the blocks in each slot.
  drawAgreeBar(s, segById, width) {
    const g = this.agree; g.replaceChildren();
    const rank = { split: 4, single: 3, partial: 2, pending: 1, all: 0, gone: -1, none: -1 };
    const worst = {};
    for (const b of s.blocks) {
      const seg = segById[b.segment];
      if (seg && seg.state === 'orphaned') continue;
      if (!(b.slot in worst) || rank[b.agreement] > rank[worst[b.slot]]) worst[b.slot] = b.agreement;
    }
    const w = Math.max(this.pxPerSlot - 1, 1);
    for (const [slot, st] of Object.entries(worst)) {
      const sl = +slot; if (sl < this.view.from || sl > this.view.to) continue;
      el('rect', { x: this.x(sl) - this.pxPerSlot / 2, y: 0, width: w, height: AGREE_H, fill: AGREE[st], opacity: st === 'pending' ? 0.25 : st === 'partial' ? 0.5 : 1 }, g);
    }
  }

  // The I* boundary: the river is tinted MPT-blue before it and PBT-orange
  // from it on, with a heavy rule and a label; the strip gets the rule too.
  drawTint(s, riverH, width) {
    const g = this.layers.tint; g.replaceChildren();
    const fx = Math.min(Math.max(this.x(s.fork_slot), 0), width);
    el('rect', { x: 0, y: 0, width: fx, height: riverH - AXIS_H, fill: FORMAT.mpt, opacity: 0.05 }, g);
    el('rect', { x: fx, y: 0, width: width - fx, height: riverH - AXIS_H, fill: FORMAT.pbt, opacity: 0.08 }, g);
  }

  // Partition bands: the slots a partition was actually applied, hatched.
  drawBands(s, riverH) {
    const g = this.layers.bands; g.replaceChildren();
    for (const p of s.partitions) {
      const end = p.lifted_slot || s.now_slot;
      if (end < this.view.from || p.applied_slot > this.view.to) continue;
      const x0 = this.x(p.applied_slot), x1 = this.x(end);
      el('rect', { x: x0, y: 0, width: Math.max(x1 - x0, 2), height: riverH - AXIS_H, fill: 'url(#hatch)', class: 'band' + (p.lifted_slot ? '' : ' open'), 'data-tip': 'part:' + s.partitions.indexOf(p) }, g);
    }
  }

  drawFinality(s, riverH) {
    const g = this.layers.finality; g.replaceChildren();
    if (s.finalized_slot <= this.view.from) return;
    el('rect', { x: 0, y: 0, width: Math.min(this.x(s.finalized_slot), this.width), height: riverH - AXIS_H, class: 'finalized' }, g)
      .append(this.title(`finalized through slot ${s.finalized_slot} (bootnode view)`));
  }

  // Blocks and edges. LOD 0: glyph + ring; 1: small squares; 2: one bar per segment.
  drawSegments(s, segById, blockByHash, inView, lod) {
    const eg = this.layers.edges, bg = this.layers.blocks, badges = this.layers.badges;
    eg.replaceChildren(); bg.replaceChildren(); badges.replaceChildren();
    if (lod === 2) {
      for (const seg of s.segments) {
        if (seg.last_slot < this.view.from || seg.first_slot > this.view.to) continue;
        const y = this.laneY(seg.lane);
        const runs = seg.first_slot < s.fork_slot && seg.last_slot >= s.fork_slot
          ? [[seg.first_slot, s.fork_slot, 'mpt'], [s.fork_slot, seg.last_slot, 'pbt']]
          : [[seg.first_slot, seg.last_slot, seg.first_slot >= s.fork_slot ? 'pbt' : 'mpt']];
        for (const [a, b, fmt] of runs) {
          el('rect', { x: this.x(a), y: y - 4, width: Math.max(this.x(b) - this.x(a), 3), height: 8, rx: 2, fill: FORMAT[fmt], stroke: segHue(seg), 'stroke-width': seg.lane === 0 ? 0 : 1.5, opacity: seg.state === 'orphaned' ? 0.35 : 0.9 }, bg);
        }
        this.tipBadge(seg, y, badges);
      }
      return;
    }
    for (const b of s.blocks) {
      if (!inView(b.slot)) continue;
      const seg = segById[b.segment]; if (!seg) continue;
      const y = this.laneY(seg.lane), x = this.x(b.slot);
      const orphaned = seg.state === 'orphaned';
      const parent = blockByHash[b.parent];
      if (parent) {
        const pseg = segById[parent.segment];
        const px = this.x(parent.slot), py = this.laneY(pseg ? pseg.lane : seg.lane);
        const d = py === y ? `M${px},${py} L${x},${y}` : `M${px},${py} C${px + (x - px) / 2},${py} ${px + (x - px) / 2},${y} ${x},${y}`;
        el('path', { d, class: 'edge', stroke: segHue(seg), opacity: orphaned ? 0.3 : 0.8 }, eg);
      } else if (b.slot > 0 && lod === 0) {
        el('path', { d: `M${x - 10},${y} L${x - BLOCK_R},${y}`, class: 'edge stub', stroke: segHue(seg) }, eg);
      }
      const g = el('g', { class: 'block' + (orphaned ? ' orphaned' : ''), transform: `translate(${x},${y})`, 'data-tip': 'block:' + b.hash }, bg);
      if (lod === 1) {
        el('rect', { x: -2, y: -2, width: 4, height: 4, fill: FORMAT[b.format] }, g);
      } else {
        if (b.istar) {
          el('polygon', { points: `0,-${BLOCK_R + 3} ${BLOCK_R + 3},0 0,${BLOCK_R + 3} -${BLOCK_R + 3},0`, fill: FORMAT[b.format], stroke: '#222', 'stroke-width': 1.2 }, g);
          if (b.istar_orphaned) el('path', { d: `M-5,-5 L5,5 M5,-5 L-5,5`, stroke: '#e0433d', 'stroke-width': 2 }, g);
        } else {
          el('circle', { r: BLOCK_R, fill: FORMAT[b.format] }, g);
        }
        this.ring(b, g);
      }
    }
    for (const seg of s.segments) {
      if (seg.last_slot < this.view.from || seg.first_slot > this.view.to) continue;
      this.tipBadge(seg, this.laneY(seg.lane), badges);
    }
  }

  // Ring = cross-node shadow-root agreement. Arc length = coverage.
  ring(b, g) {
    const st = b.agreement; if (st === 'gone' || st === 'none') return;
    const r = BLOCK_R + 3;
    if (st === 'all') el('circle', { r, class: 'ring', stroke: AGREE.all }, g);
    else if (st === 'split') { el('circle', { r, class: 'ring', stroke: AGREE.split, 'stroke-width': 2.5 }, g); el('path', { d: `M${-r},${-r} L${r},${r}`, stroke: AGREE.split, 'stroke-width': 2 }, g); }
    else if (st === 'partial') { const k = Object.keys(b.shadow_roots).length, n = 4; const a = Math.PI * 2 * (k / n); el('path', { d: `M0,${-r} A${r},${r} 0 ${a > Math.PI ? 1 : 0} 1 ${r * Math.sin(a)},${-r * Math.cos(a)}`, class: 'ring', stroke: AGREE.partial }, g); }
    else if (st === 'single') el('circle', { r, class: 'ring dashed', stroke: AGREE.single }, g);
    else el('circle', { r, class: 'ring', stroke: AGREE.pending, opacity: 0.35 }, g);
  }

  // ⟨n⟩ blocks on this segment since its fork point; frozen once orphaned,
  // and then naming the nodes that were on it - the mark of who moved.
  tipBadge(seg, y, g) {
    if (seg.lane === 0 && seg.state === 'live') return;
    const x = this.x(seg.last_slot) - 10;
    const lost = seg.state === 'orphaned';
    const t = el('text', { x, y: y - 12, 'text-anchor': 'end', class: 'badge' + (lost ? ' lost' : ''), fill: lost ? '#e0433d' : segHue(seg) }, g);
    t.textContent = lost ? `✕ ⟨${seg.blocks}⟩ lost by ${seg.lost_by.join(',')}` : `⟨${seg.blocks}⟩`;
  }

  // Reorg arrows, persistent: a dashed rewind from the lost tip down to the
  // common ancestor, then a solid catch-up along the winner to the new head.
  // A ghost chip stays at the lost tip naming the node that moved.
  drawMoves(s, segById, blockByHash) {
    const g = this.layers.moves; g.replaceChildren();
    for (const r of s.reorgs) {
      const old = blockByHash[r.old_head], anc = blockByHash[r.ancestor], nw = blockByHash[r.new_head];
      const lostSeg = segById[r.lost_segment], wonSeg = segById[r.won_segment];
      if (!lostSeg || !wonSeg) continue;
      const yo = this.laneY(lostSeg.lane), ya = this.laneY(wonSeg.lane);
      const side = yo < ya ? 1 : -1; // island above -> route the catch-up below the winner
      const xo = old ? this.x(old.slot) : this.x(lostSeg.last_slot);
      const xa = anc ? this.x(anc.slot) : this.x(lostSeg.first_slot) - this.pxPerSlot;
      const xn = nw ? this.x(nw.slot) : this.x(r.slot);
      if (xo < -50 && xn < -50) continue;
      if (xa > this.width + 50) continue;
      const mid = (yo + ya) / 2;
      const dRewind = `M${xo},${yo + side * (BLOCK_R + 4)} C${xo},${mid} ${xa},${mid} ${xa},${ya - side * (BLOCK_R + 4)}`;
      const yc = ya + side * 13;
      const dCatch = `M${xa},${yc} L${xn},${yc}`;
      el('path', { d: dRewind, class: 'rewind', 'marker-end': 'url(#arrow-red)' }, g);
      el('path', { d: dCatch, class: 'catchup', 'marker-end': 'url(#arrow-green)' }, g);
      const hit = el('g', { class: 'move-hit', 'data-tip': 'move:' + s.reorgs.indexOf(r) }, g);
      el('path', { d: dRewind, class: 'hit' }, hit); el('path', { d: dCatch, class: 'hit' }, hit);
      // ghost of the node at the tip it left
      const ghost = el('g', { class: 'ghost', transform: `translate(${xo + 16},${yo})` }, g);
      el('circle', { r: CHIP_R - 1, class: 'ghost-body' }, ghost);
      const ghostClient = (s.nodes.find(n => n.id === r.node) || {}).client;
      if (LOGOS[ghostClient]) {
        const gs = CHIP_R * 1.3;
        el('use', { href: '#logo-' + ghostClient, class: 'ghost-logo', x: -gs / 2, y: -gs / 2, width: gs, height: gs }, ghost);
        el('text', { x: CHIP_R - 2, y: CHIP_R, 'text-anchor': 'middle', class: 'ghost-label small' }, ghost).textContent = r.node;
      } else el('text', { y: 4, 'text-anchor': 'middle', class: 'ghost-label' }, ghost).textContent = r.node;
      if (xn - xa >= 110) {
        const label = el('text', { x: xn - 8, y: yc + (side > 0 ? 12 : -5), class: 'move-label', 'text-anchor': 'end' }, g);
        label.textContent = `${r.node} ⤺${r.depth} ▸+${r.added}${r.crosses_istar ? ' ↩I*' : ''}`;
      }
      if (anc && xn - xa >= 60) el('text', { x: xa, y: ya + side * 26, class: 'axis-label', 'text-anchor': 'middle' }, g).textContent = `#${r.ancestor_number}`;
    }
  }

  moveCard(r, s, segById) {
    const lost = segById[r.lost_segment];
    return `<b>node ${r.node} moved</b> · slot ${r.slot} (${clock(r.slot, s)})<div>left ${r.lost_segment} (${lost ? lost.blocks : '?'} blocks, lost) · rewound ${r.depth} to #${r.ancestor_number}</div><div>joined ${r.won_segment} (wins) · +${r.added} blocks to catch the head</div>${r.crosses_istar ? '<div class="bad">crossed I* backwards to MPT blocks, re-crossed on the winner\'s I*</div>' : ''}`;
  }

  // Node chips ride their heads; stacked when several share a head.
  drawChips(s, prev, segById, blockByHash) {
    const g = this.layers.chips; const cg = this.layers.cursors; cg.replaceChildren();
    const byHead = {};
    for (const n of s.nodes) (byHead[n.head] ||= []).push(n);
    const seen = new Set();
    for (const [hash, nodes] of Object.entries(byHead)) {
      const b = blockByHash[hash]; const seg = b ? segById[b.segment] : segById[nodes[0].segment];
      const slot = b ? b.slot : nodes[0].head_slot;
      const y = this.laneY(seg ? seg.lane : 0);
      nodes.forEach((n, i) => {
        const id = 'chip-' + n.id; seen.add(id);
        let chip = g.querySelector('#' + id);
        const x = this.x(slot) + 16 + i * (CHIP_R * 2 + 3);
        if (!chip) {
          chip = el('g', { id, class: 'chip', 'data-tip': 'node:' + n.id }, g);
          el('circle', { r: CHIP_R, class: 'chip-body' }, chip);
          el('circle', { r: CHIP_R + 3, class: 'chip-istar' }, chip);
          if (LOGOS[n.client]) {
            const s = CHIP_R * 1.5;
            el('use', { href: '#logo-' + n.client, class: 'chip-logo', x: -s / 2, y: -s / 2, width: s, height: s }, chip);
            const badge = el('g', { class: 'chip-badge' }, chip);
            el('circle', { r: 5.5, cx: CHIP_R - 1, cy: CHIP_R - 1, class: 'chip-badge-body' }, badge);
            el('text', { x: CHIP_R - 1, y: CHIP_R + 1.6, 'text-anchor': 'middle', class: 'chip-badge-label' }, badge).textContent = n.id;
          } else {
            el('text', { class: 'chip-label', y: 4, 'text-anchor': 'middle' }, chip).textContent = n.id;
          }
          chip.setAttribute('transform', `translate(${x},${y})`);
        }
        // slide only when the head moved; pan/zoom re-renders snap
        const was = prev && prev.nodes.find(p => p.id === n.id);
        chip.style.transition = was && was.head !== n.head ? 'transform 600ms ease' : 'none';
        chip.setAttribute('transform', `translate(${x},${y})`);
        const body = chip.querySelector('.chip-body');
        body.setAttribute('fill', PHASE[n.phase] || PHASE.unknown);
        const logo = chip.querySelector('.chip-logo');
        if (logo) logo.setAttribute('color', glyphInk(n.phase));
        body.setAttribute('stroke', seg ? segHue(seg) : '#222');
        body.classList.toggle('unreachable', n.status !== 'ok');
        chip.querySelector('.chip-istar').setAttribute('class', 'chip-istar ' + n.istar);
        if (n.lag > 0 || n.cursor_detached) {
          const cx = this.x(slot - n.lag);
          el('line', { x1: cx, y1: y + 14, x2: this.x(slot), y2: y + 14, class: 'tether' }, cg);
          el('path', { d: `M${cx},${y + 9} L${cx + 5},${y + 14} L${cx},${y + 19} Z`, fill: PHASE[n.phase] || '#888', class: 'cursor' + (n.cursor_detached ? ' detached' : '') }, cg);
        }
      });
    }
    for (const c of Array.from(g.children)) if (!seen.has(c.id)) c.remove();
  }

  // Axis: the I* rule with its label, "now", and height labels along the
  // canonical lane thinned to ~70px apart.
  drawAxis(s, riverH, width) {
    const g = this.layers.axis; g.replaceChildren();
    const fx = this.x(s.fork_slot);
    if (fx >= -100 && fx <= width + 100) {
      el('line', { x1: fx, x2: fx, y1: 0, y2: riverH - AXIS_H, class: 'fork-rule heavy' }, g);
      const lbl = el('g', { transform: `translate(${fx},2)` }, g);
      const box = el('rect', { x: -2, y: 0, width: 200, height: 18, rx: 3, fill: '#222' }, lbl);
      const txt = el('text', { x: 4, y: 13, class: 'fork-label' }, lbl);
      txt.textContent = `I* · slot ${s.fork_slot} · ${clock(s.fork_slot, s)} · MPT → PBT`;
      box.setAttribute('width', txt.getComputedTextLength() + 12);
    }
    const nx = this.x(s.now_slot);
    el('line', { x1: nx, x2: nx, y1: 0, y2: riverH - AXIS_H, class: 'now-rule' }, g);
    const y = riverH - 4;
    const spineIds = new Set(s.segments.filter(x => x.lane === 0).map(x => x.id));
    const spine = s.blocks.filter(b => spineIds.has(b.segment));
    if (spine.length < 2) return;
    const slotsPerBlock = (spine[spine.length - 1].slot - spine[0].slot) / (spine.length - 1) || 1;
    const step = [10, 20, 50, 100, 200, 500].find(st => st * slotsPerBlock * this.pxPerSlot >= 70) || 1000;
    for (const b of spine) {
      if (b.number % step !== 0 || b.slot < this.view.from || b.slot > this.view.to) continue;
      el('text', { x: this.x(b.slot), y, class: 'axis-label', 'text-anchor': 'middle' }, g).textContent = '#' + b.number;
    }
  }

  // Strip: one row per node on the same time axis. Fill = phase, hatch =
  // isolated, darker left of its own finalized slot, 2px line = segment hue.
  drawStrip(s, segById, width, stripH) {
    const g = this.strip; g.replaceChildren();
    s.nodes.forEach((n, i) => {
      const y = i * STRIP_ROW + 2;
      const seg = segById[n.segment];
      el('rect', { x: 0, y, width, height: STRIP_ROW - 3, fill: PHASE[n.phase] || PHASE.unknown, opacity: 0.35, rx: 3 }, g);
      if (n.finalized_slot > this.view.from) el('rect', { x: 0, y, width: Math.min(this.x(n.finalized_slot), width), height: STRIP_ROW - 3, fill: 'rgba(0,0,0,0.08)', rx: 3 }, g);
      for (const p of s.partitions) {
        if (!p.victims.includes(n.id)) continue;
        const end = p.lifted_slot || s.now_slot;
        if (end < this.view.from || p.applied_slot > this.view.to) continue;
        el('rect', { x: this.x(p.applied_slot), y, width: Math.max(this.x(end) - this.x(p.applied_slot), 2), height: STRIP_ROW - 3, fill: 'url(#hatch)' }, g);
      }
      el('line', { x1: 0, x2: this.x(n.head_slot), y1: y + STRIP_ROW - 6, y2: y + STRIP_ROW - 6, stroke: seg ? segHue(seg) : '#222', 'stroke-width': 2 }, g);
      for (const r of s.reorgs) {
        if (r.node !== n.id || r.slot < this.view.from || r.slot > this.view.to) continue;
        el('text', { x: this.x(r.slot), y: y + 15, class: 'rewind', 'text-anchor': 'middle' }, g).textContent = `⤺${r.depth}`;
      }
      const label = el('g', { class: 'strip-label', transform: `translate(6,${y + 15})` }, g);
      el('rect', { x: -4, y: -12, width: 168, height: 17, rx: 3, fill: 'rgba(255,255,255,0.85)' }, label);
      if (LOGOS[n.client]) el('use', { href: '#logo-' + n.client, class: 'strip-logo', x: -2, y: -10, width: 11, height: 11 }, label);
      el('text', { class: 'strip-text', x: LOGOS[n.client] ? 13 : 0 }, label).textContent = `${n.id}  #${n.head_number}  lag ${n.lag}`;
      this.peerBars(label, 118, n.el_peers, 3, '#4c8dff'); this.peerBars(label, 144, n.cl_peers, 3, '#8a5cff');
      if (n.status !== 'ok') el('rect', { x: 0, y, width, height: STRIP_ROW - 3, fill: 'url(#hatch-red)', rx: 3 }, g);
    });
    el('line', { x1: this.x(s.fork_slot), x2: this.x(s.fork_slot), y1: 0, y2: stripH, class: 'fork-rule heavy' }, g);
  }

  peerBars(g, x, count, max, color) {
    for (let i = 0; i < max; i++) el('rect', { x: x + i * 6, y: -9, width: 4, height: 10, rx: 1, fill: i < count ? color : '#ddd' }, g);
  }

  // A chip whose node just reorged gets a −N badge for 30s.
  animateReorgs(s, prev) {
    if (!prev) return;
    for (const r of s.reorgs) {
      if (prev.reorgs.some(p => p.slot === r.slot && p.node === r.node)) continue;
      const chip = this.layers.chips.querySelector('#chip-' + r.node); if (!chip) continue;
      const t = el('text', { class: 'reorg-badge', x: 0, y: -14, 'text-anchor': 'middle' }, chip);
      t.textContent = `−${r.depth}`;
      setTimeout(() => t.remove(), 30000);
    }
  }

  // --- panels -------------------------------------------------------------

  blockTip(b, seg) {
    const primary = b.format.toUpperCase(), shadow = OTHER[b.format].toUpperCase();
    return `<b>#${b.number}</b> · slot ${b.slot} · ${primary} primary${b.istar ? ' · I*' : ''}${seg.state === 'orphaned' ? ' · <span class="bad">orphaned</span>' : ''}<div class="mono">${short(b.hash)}</div><div>${shadow} shadow: <span class="agree-${b.agreement}">${b.agreement}</span>${b.dissent.length ? ' · dissent node ' + b.dissent.join(',') : ''}</div><div class="muted">click to pin in the inspector</div>`;
  }

  // The inspector: one block in full - which root is primary, which is the
  // shadow, who agrees with the majority, who has finalized it.
  renderBlockCard(b, seg, s) {
    const p = this.panels.block; if (!p) return;
    const primary = b.format.toUpperCase(), shadow = OTHER[b.format].toUpperCase();
    const { classes, missing } = rootClasses(b, s);
    const orphaned = seg.state === 'orphaned';
    const finalizedBy = s.nodes.filter(n => n.finalized_slot >= b.slot && !orphaned).map(n => n.id);
    const notFinal = s.nodes.filter(n => !finalizedBy.includes(n.id)).map(n => n.id);
    const rows = [];
    rows.push(`<div class="card-head"><b>block #${b.number}</b> · slot ${b.slot} (${clock(b.slot, s)}) · ${b.istar ? '<b>I*</b> · ' : ''}${orphaned ? `<span class="bad">orphaned</span> · ${seg.id}, lost by node ${seg.lost_by.join(',')}` : `canonical · ${seg.id}`}${this.pinned === b.hash ? ' · <span class="muted">pinned</span>' : ''}</div>`);
    rows.push(`<div class="kv"><span>hash</span><span class="mono">${b.hash}</span></div>`);
    rows.push(`<div class="kv"><span>primary <i class="sw" style="background:${FORMAT[b.format]}"></i>${primary}</span><span class="mono">${b.state_root}<span class="muted"> header root, ${b.format === 'mpt' ? 'before' : 'after'} I*</span></span></div>`);
    if (!classes.length) rows.push(`<div class="kv"><span>shadow <i class="sw" style="background:${FORMAT[OTHER[b.format]]}"></i>${shadow}</span><span class="muted">no node has reported yet</span></div>`);
    classes.forEach((c, i) => {
      const majority = i === 0 && classes.length > 1 ? ' majority' : i === 0 ? '' : ' dissent';
      rows.push(`<div class="kv"><span>${i === 0 ? `shadow <i class="sw" style="background:${FORMAT[OTHER[b.format]]}"></i>${shadow}` : ''}</span><span class="mono ${i === 0 ? 'ok' : 'bad'}">${c.root}<span class="muted"> nodes ${c.nodes.join(', ')}${majority}${orphaned ? ' · before the branch lost' : ''}</span></span></div>`);
    });
    if (missing.length && b.agreement !== 'gone') rows.push(`<div class="kv"><span></span><span class="muted">not reported by node ${missing.join(', ')}</span></div>`);
    rows.push(`<div class="kv"><span>agreement</span><span class="agree-${b.agreement}">${b.agreement}</span></div>`);
    rows.push(`<div class="kv"><span>finalized by</span><span>${orphaned ? '<span class="muted">never - the branch lost</span>' : (finalizedBy.length ? 'node ' + finalizedBy.join(', ') : '—') + (notFinal.length ? ` <span class="muted">· not yet: node ${notFinal.join(', ')}</span>` : '')}</span></div>`);
    const moves = s.reorgs.filter(r => r.old_head === b.hash || r.ancestor === b.hash || r.new_head === b.hash);
    for (const r of moves) rows.push(`<div class="kv"><span>move</span><span>node ${r.node} ${r.old_head === b.hash ? 'left this tip' : r.ancestor === b.hash ? 'rewound to here' : 'caught up here'} at slot ${r.slot} (⤺${r.depth}, +${r.added}${r.crosses_istar ? ', ↩I*' : ''})</span></div>`);
    p.innerHTML = rows.join('');
  }

  // Every head move, newest first: who left which chain, where it rewound
  // to, which chain won. Click a row to pan the river there.
  renderMoves(s, segById) {
    const p = this.panels.moves; if (!p) return;
    p.replaceChildren();
    for (const r of s.reorgs.slice().reverse()) {
      const lost = segById[r.lost_segment];
      const li = document.createElement('li');
      li.innerHTML = `<span class="mono">${String(r.slot).padStart(4)}</span> node <b>${r.node}</b> ⤺${r.depth} <span class="muted">${r.lost_segment}${lost ? ' ⟨' + lost.blocks + '⟩' : ''} lost</span> → <b>${r.won_segment}</b> wins at #${r.ancestor_number}, +${r.added}${r.crosses_istar ? ' <span class="bad">↩ I*</span>' : ''}`;
      li.onclick = () => { const span = this.view.to - this.view.from; this.view = { from: r.slot - span * 0.6, to: r.slot + span * 0.4, follow: false }; this.render(this.prev); };
      p.appendChild(li);
    }
    if (!s.reorgs.length) p.innerHTML = '<li class="muted">no head moves yet</li>';
  }

  title(text) { const t = el('title'); t.textContent = text; return t; }

  // Tooltip and card are keyed (kind:id), never element-bound: a re-render
  // replaces the SVG under a still pointer, so both are rebuilt from the
  // current snapshot on every tick and go away when the key disappears.
  nodeTip(n, s) {
    return `<b>node ${n.id}</b> · ${n.phase}${n.isolated ? ' · <span class="bad">isolated</span>' : ''}${n.status !== 'ok' ? ' · <span class="bad">' + n.status + '</span>' : ''}<div>head #${n.head_number} ${short(n.head)} on ${n.segment}</div><div>PBT cursor #${n.cursor_number} (lag ${n.lag}) · I*: ${n.istar}</div><div>peers EL ${n.el_peers} · CL ${n.cl_peers} · finalized slot ${n.finalized_slot}</div>`;
  }
  tipHtml(key) {
    const s = this.s; if (!s || !key) return null;
    const [kind, id] = key.split(/:(.*)/);
    if (kind === 'block') { const b = this.blockByHash[id]; return b ? this.blockTip(b, this.segById[b.segment]) : null; }
    if (kind === 'node') { const n = s.nodes.find(x => x.id === +id); return n ? this.nodeTip(n, s) : null; }
    if (kind === 'op') { const op = s.schedule[+id]; return op ? this.opCard(op, s) : null; }
    if (kind === 'move') { const r = s.reorgs[+id]; return r ? this.moveCard(r, s, this.segById) : null; }
    if (kind === 'part') { const p = s.partitions[+id]; return p ? `<b>${esc(p.class)} partition · ${esc(p.name)}</b><div>node ${p.victims.join(', ')} cut off (EL+CL p2p) · slots ${p.applied_slot}–${p.lifted_slot || 'now'}</div>` : null; }
    return null;
  }
  refreshTip() {
    const html = this.tipHtml(this.tipKey);
    if (html == null) { this.hideTip(); return; }
    this.panels.tip.innerHTML = html; this.panels.tip.style.display = 'block';
  }
  moveTip(ev) { const t = this.panels.tip; const x = Math.min(ev.clientX + 12, window.innerWidth - 440); t.style.left = x + 'px'; t.style.top = (ev.clientY + 12) + 'px'; }
  hideTip() { this.tipKey = null; this.panels.tip.style.display = 'none'; }
  // The card follows the pinned block, else the last hovered one.
  refreshCard() {
    const hash = this.pinned || this.hovered; if (!hash) return;
    const b = this.blockByHash[hash];
    if (b) this.renderBlockCard(b, this.segById[b.segment], this.s);
    else if (this.panels.block) this.panels.block.innerHTML = `<span class="placeholder">block ${short(hash)} is no longer in the served window</span>`;
  }
  onHover(ev) {
    const t = ev.target.closest ? ev.target.closest('[data-tip]') : null;
    const key = t ? t.getAttribute('data-tip') : null;
    if (key !== this.tipKey) {
      this.tipKey = key;
      this.refreshTip();
      if (key && key.startsWith('block:') && !this.pinned) { this.hovered = key.slice(6); this.refreshCard(); }
    }
    if (key) this.moveTip(ev);
  }

  bindPanZoom() {
    let drag = null;
    this.svg.addEventListener('wheel', (ev) => {
      ev.preventDefault();
      const span = this.view.to - this.view.from;
      const factor = ev.deltaY > 0 ? 1.15 : 1 / 1.15;
      const next = Math.min(Math.max(span * factor, 20), 1200);
      const anchor = this.view.from + (ev.offsetX - 24) / this.pxPerSlot;
      const ratio = (anchor - this.view.from) / span;
      this.view.from = anchor - next * ratio; this.view.to = this.view.from + next;
      this.view.follow = this.view.to >= (this.prev ? this.prev.now_slot : 0);
      if (this.prev) this.render(this.prev);
    }, { passive: false });
    this.svg.addEventListener('mousedown', (ev) => { drag = { x: ev.clientX, from: this.view.from, to: this.view.to, moved: false }; });
    window.addEventListener('mousemove', (ev) => {
      if (!drag) return;
      const d = (drag.x - ev.clientX) / this.pxPerSlot;
      if (Math.abs(drag.x - ev.clientX) > 3) drag.moved = true;
      this.view.from = drag.from + d; this.view.to = drag.to + d; this.view.follow = false;
      if (this.prev) this.render(this.prev);
    });
    window.addEventListener('mouseup', () => { setTimeout(() => { drag = null; }, 0); });
    this.svg.addEventListener('dblclick', () => { this.view.follow = true; if (this.prev) this.render(this.prev); });
    this.root.addEventListener('mousemove', (ev) => { if (!drag) this.onHover(ev); });
    this.root.addEventListener('mouseleave', () => this.hideTip());
    this.svg.addEventListener('click', (ev) => {
      if (drag && drag.moved) return;
      const b = ev.target.closest('.block');
      if (b) { const hash = b.getAttribute('data-tip').slice(6); this.pinned = this.pinned === hash ? null : hash; this.hovered = hash; }
      else this.pinned = null;
      this.refreshCard();
    });
  }
}
