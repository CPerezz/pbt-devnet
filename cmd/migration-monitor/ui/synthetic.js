// Synthetic lap: a scripted four-node migration run that exercises every
// glyph the page draws. Deterministic (seeded), so a review compares the
// same picture; produces the same snapshot shape /api/state will serve.
//
// Timeline (slots, 6s each): genesis 0; deep partitions on node 2 (40-72,
// 90-122); short on node 3 (132-157); node 4 unreachable 200-215; straddle
// on node 3 across I*=300 (285-315: two I* candidates, node 3's orphaned);
// window partition on node 4 (340-365); post-fork deep and two scenarios.
// One injected shadow-root split at block #150 (node 4 dissents).

export function syntheticLap(nowSlot) {
  let seed = 7;
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff;
  const hex = (tag, n) => {
    let tagMix = 0; for (const ch of tag) tagMix = (tagMix * 131 + ch.charCodeAt(0)) >>> 0;
    let s = (0x9e3779b9 ^ (n * 2654435761) ^ (tagMix * 40503)) >>> 0;
    let out = '0x';
    for (let i = 0; i < 8; i++) { s = (s * 1664525 + 1013904223) >>> 0; out += s.toString(16).padStart(8, '0'); }
    return out;
  };

  const T = 300, FINALITY_LAG = 30, SLOT_SECONDS = 6;
  const stake = { 1: 0.2, 2: 0.4, 3: 0.2, 4: 0.2 };
  const nodes = [1, 2, 3, 4];

  const plan = [
    { name: 'deep-1', class: 'deep', victims: [2], start: 40, end: 72 },
    { name: 'deep-2', class: 'deep', victims: [2], start: 90, end: 122 },
    { name: 'short-1', class: 'short', victims: [3], start: 132, end: 157 },
    { name: 'straddle', class: 'straddle', victims: [3], start: 285, end: 315 },
    { name: 'window-1', class: 'window', victims: [4], start: 340, end: 365 },
    { name: 'post-deep', class: 'deep', victims: [2], start: 400, end: 432 },
    { name: 'code-sole', class: 'scenario', victims: [3], start: 445, end: 452 },
    { name: 'account', class: 'scenario', victims: [4], start: 470, end: 478 },
  ];
  const schedule = plan.map(p => ({ name: p.name, class: p.class, victims: p.victims, start_slot: p.start, end_slot: p.end }));
  const partitions = plan.filter(p => p.start <= nowSlot).map(p => ({ name: p.name, class: p.class, victims: p.victims, applied_slot: p.start, lifted_slot: p.end <= nowSlot ? p.end : 0 }));

  // Chain construction: one canonical spine plus island branches minted by
  // the isolated victim during each partition, orphaned at the heal.
  const blocks = [];
  const segments = [];
  const reorgs = [];
  const alerts = [];
  let number = 0;
  const spineSeg = { id: 's0', lane: 0, ancestor: null, first_slot: 0, last_slot: 0, blocks: 0, tip: null, state: 'live', holders: [1, 2, 3, 4], lost_by: [] };
  segments.push(spineSeg);
  let prevHash = hex('g', 0);

  // Lane allocation mirrors the server: smallest free lane, alternating
  // above/below the spine; a lane is free once its last occupant ended.
  const freeLane = (slot) => {
    for (const lane of [1, -1, 2, -2, 3, -3]) {
      if (segments.every(s => s.lane !== lane || (s.state !== 'live' && s.last_slot < slot - 3))) return lane;
    }
    return 4;
  };

  const mint = (slot, seg, parent, num, format) => {
    const h = hex(seg.id, num * 1000 + slot);
    const b = { hash: h, parent, number: num, slot, segment: seg.id, format, istar: false, istar_orphaned: false,
      state_root: hex(format + '-primary-' + seg.id, num), shadow_roots: {}, agreement: 'pending', dissent: [] };
    blocks.push(b);
    seg.blocks++; seg.last_slot = slot; seg.tip = h;
    return b;
  };

  const islands = []; // {seg, parentHash, ancestorHash, ancestorNumber, number, victim, crossed, closed}
  let crossedT = false;
  const istarByNode = {};
  const activeVictims = (slot) => partitions.filter(p => p.applied_slot <= slot && (p.lifted_slot === 0 || slot < p.lifted_slot)).flatMap(p => p.victims);

  for (let slot = 1; slot <= nowSlot; slot++) {
    const victims = activeVictims(slot);
    const majorityStake = nodes.filter(n => !victims.includes(n)).reduce((a, n) => a + stake[n], 0);
    const format = slot >= T ? 'pbt' : 'mpt';

    if (rnd() < majorityStake) {
      number++;
      const b = mint(slot, spineSeg, prevHash, number, format);
      if (!crossedT && slot >= T) { b.istar = true; crossedT = true; nodes.filter(n => !victims.includes(n)).forEach(n => istarByNode[n] = 'provisional'); }
      prevHash = b.hash;
    }
    for (const v of victims) {
      let isl = islands.find(i => i.victim === v && !i.closed);
      if (!isl) {
        const seg = { id: 's' + segments.length, lane: freeLane(slot), ancestor: prevHash, first_slot: slot, last_slot: slot, blocks: 0, tip: null, state: 'live', holders: [v], lost_by: [] };
        segments.push(seg);
        // only a branch forked below I* can cross it on its own block
        isl = { seg, parentHash: prevHash, ancestorHash: prevHash, ancestorNumber: number, number, victim: v, crossed: slot > T, closed: false };
        islands.push(isl);
      }
      if (rnd() < stake[v]) {
        isl.number++;
        const b = mint(slot, isl.seg, isl.parentHash, isl.number, format);
        if (!isl.crossed && slot >= T) { b.istar = true; isl.crossed = true; istarByNode[v] = 'provisional'; }
        isl.parentHash = b.hash;
      }
    }
    // heals: an island whose victim is no longer isolated loses to the spine
    for (const isl of islands) {
      if (isl.closed || victims.includes(isl.victim)) continue;
      isl.closed = true;
      const dropped = isl.seg.blocks;
      if (dropped === 0) { segments.splice(segments.indexOf(isl.seg), 1); continue; } // never diverged
      isl.seg.state = 'orphaned'; isl.seg.holders = []; isl.seg.lost_by = [isl.victim];
      reorgs.push({ slot, node: isl.victim, depth: dropped, added: number - isl.ancestorNumber,
        ancestor: isl.ancestorHash, ancestor_number: isl.ancestorNumber, old_head: isl.seg.tip, new_head: prevHash,
        lost_segment: isl.seg.id, won_segment: 's0', crosses_istar: isl.seg.first_slot <= T && slot >= T });
      blocks.filter(b => b.segment === isl.seg.id && b.istar).forEach(b => { b.istar_orphaned = true; });
      alerts.push({ slot, kind: 'warn', node: isl.victim, expected: true, detail: `canonical split healed: node ${isl.victim} rewound ${dropped} blocks to #${isl.ancestorNumber}` });
    }
  }

  // Shadow roots: everything ≥12 slots old that the majority holds agrees,
  // fresher blocks are partial/pending; one injected dissent for the legend.
  for (const b of blocks) {
    const age = nowSlot - b.slot;
    const seg = segments.find(s => s.id === b.segment);
    const shadow = hex(b.format === 'mpt' ? 'pbt-shadow' : 'mpt-shadow', b.number);
    if (seg.state === 'orphaned') { b.agreement = 'gone'; continue; }
    const holders = seg.holders;
    if (holders.length === 1) { b.agreement = age > 4 ? 'single' : 'pending'; if (age > 4) b.shadow_roots[holders[0]] = shadow; continue; }
    if (age > 12) { b.agreement = 'all'; holders.forEach(n => b.shadow_roots[n] = shadow); }
    else if (age > 4) { b.agreement = 'partial'; holders.slice(0, 2).forEach(n => b.shadow_roots[n] = shadow); }
  }
  const splitAt = blocks.find(b => b.segment === 's0' && b.number === 150);
  if (splitAt) {
    splitAt.agreement = 'split'; splitAt.shadow_roots[4] = hex('x', 150); splitAt.dissent = [4];
    alerts.push({ slot: splitAt.slot, kind: 'critical', node: 4, expected: false, detail: 'PBT shadow root mismatch at block #150 (node 4 vs majority)' });
  }

  const finalized = Math.max(0, nowSlot - FINALITY_LAG);
  const nodeViews = nodes.map(n => {
    const isolated = activeVictims(nowSlot).includes(n);
    const isl = islands.find(i => i.victim === n && !i.closed);
    const seg = isl ? isl.seg : spineSeg;
    const headBlock = blocks.filter(b => b.segment === seg.id).slice(-1)[0] || blocks[0];
    const phase = nowSlot < T ? (n === 3 && nowSlot < 20 ? 'following' : 'synced') : (nowSlot < T + 60 ? 'window' : 'done');
    const lag = nowSlot < T ? (n === 3 ? 3 : 0) : 0;
    return {
      id: n, name: `el-${n}-geth-lighthouse`, head: headBlock.hash, head_number: headBlock.number, head_slot: headBlock.slot, segment: seg.id,
      phase, cursor_number: headBlock.number - lag, cursor_hash: headBlock.hash, lag, cursor_detached: false,
      istar: nowSlot < T ? 'none' : (finalized >= T ? 'final' : (istarByNode[n] || 'provisional')),
      el_peers: isolated ? 0 : 3, cl_peers: isolated ? 0 : 3, finalized_slot: isolated ? Math.max(0, seg.first_slot - 5) : finalized,
      status: nowSlot > 200 && nowSlot < 215 && n === 4 ? 'unreachable' : 'ok', isolated,
    };
  });
  for (const s of segments) if (s.state === 'live' && s.id !== 's0') s.holders = nodeViews.filter(v => v.segment === s.id).map(v => v.id);

  return {
    seq: nowSlot, now_slot: nowSlot, slot_seconds: SLOT_SECONDS, fork_slot: T, finalized_slot: finalized,
    nodes: nodeViews, segments, blocks, reorgs, partitions, schedule, alerts,
  };
}
