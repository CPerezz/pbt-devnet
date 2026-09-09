// Slot River: the migration devnet as a block DAG on a time axis, one lane
// per lineage segment, node chips riding their heads, and a per-node strip
// beneath. Pure render(snapshot, prev): the server has already derived
// lineage, depth and agreement; this file only draws.

const NS = 'http://www.w3.org/2000/svg';
const LANE_H = 38, BLOCK_R = 6, STRIP_ROW = 26, RIBBON_H = 22, AGREE_H = 8, AXIS_H = 18, TOP_PAD = 26;
const FORMAT = { mpt: '#4c8dff', pbt: '#ff9a3c' };
const OTHER = { mpt: 'pbt', pbt: 'mpt' };
const PHASE = { following: '#9fd6a3', synced: '#2fa84f', parked: '#8d8d8d', window: '#ffc27a', done: '#ff8a1f', stalled: '#e0433d', unknown: '#c8c8c8' };
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
const CHIP_R = 9;

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
      el('text', { y: 4, 'text-anchor': 'middle', class: 'ghost-label' }, ghost).textContent = r.node;
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
          el('text', { class: 'chip-label', y: 4, 'text-anchor': 'middle' }, chip).textContent = n.id;
          chip.setAttribute('transform', `translate(${x},${y})`);
        }
        // slide only when the head moved; pan/zoom re-renders snap
        const was = prev && prev.nodes.find(p => p.id === n.id);
        chip.style.transition = was && was.head !== n.head ? 'transform 600ms ease' : 'none';
        chip.setAttribute('transform', `translate(${x},${y})`);
        const body = chip.querySelector('.chip-body');
        body.setAttribute('fill', PHASE[n.phase] || PHASE.unknown);
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
      el('text', { class: 'strip-text' }, label).textContent = `${n.id}  #${n.head_number}  lag ${n.lag}`;
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
