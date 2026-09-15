// Synthetic lap: a scripted four-node migration run that exercises every
// glyph the page draws. Deterministic (seeded), so a review compares the
// same picture; produces the same snapshot shape /api/state will serve.
//
// The schedule below is not invented: it is what
// migsched.Resolve("composite", genesis, genesis+1800s, {anchor 1, lights
// 2,3,4}) actually returns for the devnet's own defaults, slot for slot.
// Stake likewise - the anchor holds 256 validators against 128 per light
// (main.star DEFAULT_MIGRATION), so no light outweighs it and a pair stalls
// finality without winning.
//
// Timeline (slots, 6s each), I* at 300: deep on the pair 2+3 (40-71) and the
// pair 3+4 (90-121), each pair sharing ONE branch; short on node 2 (131-156);
// node 4 restarted at 180, the lap's crash drill; the straddle (280-310)
// isolating every light from everyone INCLUDING each other, so each crosses
// I* on its own block - four candidates, three orphaned; window partition on
// node 3 (350-375); then the post-quiesce scenarios, which are unscheduled
// and so reach the page as class `scenario` with no planned band.
//
// Divergence is lazy, as it is on the wire: a cut-off node keeps the head it
// had and only leaves the canonical chain when it proposes. In the lap this
// page mirrors, the straddle isolated 2, 3 and 4 in the same instant and
// their heads peeled off 1, 4 and 5 slots later.
//
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
  const stake = { 1: 0.4, 2: 0.2, 3: 0.2, 4: 0.2 };
  const nodes = [1, 2, 3, 4];
  // The migration profile's own layout, so the preview shows the marks the live page does.
  const CLIENTS = ['geth', 'erigon', 'geth', 'besu'];

  // The five scheduled ops, verbatim from the resolver.
  const plan = [
    { name: 'mig-composite-1', class: 'deep', victims: [2, 3], start: 40, end: 71 },
    { name: 'mig-composite-2', class: 'deep', victims: [3, 4], start: 90, end: 121 },
    { name: 'mig-composite-3', class: 'short', victims: [2], start: 131, end: 156 },
    { name: 'mig-composite-4', class: 'straddle', victims: [2, 3, 4], start: 280, end: 310, mutual: true },
    { name: 'mig-composite-5', class: 'window', victims: [3], start: 350, end: 375 },
  ];
  // Post-quiesce scenarios: pbtchaos isolates one node in rotation to a bounded
  // depth. They are not in the schedule, which is exactly why the monitor
  // classes them `scenario` (feeds.go:151-162) - so they get no planned band.
  const unscheduled = [
    { name: 'code-sole', class: 'scenario', victims: [4], start: 400, end: 413 },
    { name: 'account', class: 'scenario', victims: [2], start: 430, end: 443 },
    { name: 'storage-del', class: 'scenario', victims: [3], start: 460, end: 473 },
  ];
  const ops = plan.concat(unscheduled);
  const schedule = plan.map(p => ({ name: p.name, class: p.class, victims: p.victims, start_slot: p.start, end_slot: p.end }));
  const partitions = ops.filter(p => p.start <= nowSlot).map(p => ({ name: p.name, class: p.class, victims: p.victims, applied_slot: p.start, lifted_slot: p.end <= nowSlot ? p.end : 0 }));

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

  // An island is one branch and the nodes building it. A deep, short or window
  // op cuts its victims off from the majority but not from each other, so they
  // share one branch and rewind together; the straddle is mutual, so each light
  // is alone and gets its own branch and its own I* candidate
  // (migration-chaos/main.go:167).
  //
  // An island exists only once someone has proposed on it. Until then the
  // cut-off node is not on a branch at all - it is sitting on the last block it
  // received, falling behind - which is why the lanes appear one at a time
  // rather than the moment the partition lands.
  const islands = []; // {key, holders, seg, parentHash, ancestorHash, ancestorNumber, number, crossed, closed}
  let crossedT = false;
  const istarByNode = {};
  const activeOps = (slot) => partitions.filter(p => p.applied_slot <= slot && (p.lifted_slot === 0 || slot < p.lifted_slot));
  const activeVictims = (slot) => activeOps(slot).flatMap(p => p.victims);
  // Who could build which branch this slot: one group per op, or one per victim
  // when the op is mutual.
  const activeGroups = (slot) => activeOps(slot).flatMap(p => {
    const op = ops.find(o => o.name === p.name);
    return op && op.mutual ? p.victims.map(v => ({ key: p.name + ':' + v, holders: [v] })) : [{ key: p.name, holders: p.victims }];
  });
  // Where a cut-off node sits before its island has a block: the chain tip at
  // the moment it was isolated.
  const strandedAt = {}; // node -> {hash, number}

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
    for (const g of activeGroups(slot)) {
      let isl = islands.find(i => i.key === g.key && !i.closed);
      if (!isl) {
        // Cut off, nothing minted yet: the node holds the tip it was stranded on.
        isl = { key: g.key, holders: g.holders.slice(), seg: null, parentHash: prevHash, ancestorHash: prevHash, ancestorNumber: number, number, crossed: slot > T, closed: false };
        islands.push(isl);
        for (const v of g.holders) strandedAt[v] = { hash: prevHash, number };
      }
      if (rnd() >= isl.holders.reduce((a, n) => a + stake[n], 0)) continue;
      if (!isl.seg) { // first proposal: the branch, and its lane, start here
        isl.seg = { id: 's' + segments.length, lane: freeLane(slot), ancestor: isl.ancestorHash, first_slot: slot, last_slot: slot, blocks: 0, tip: null, state: 'live', holders: isl.holders.slice(), lost_by: [] };
        segments.push(isl.seg);
      }
      isl.number++;
      const b = mint(slot, isl.seg, isl.parentHash, isl.number, format);
      if (!isl.crossed && slot >= T) { b.istar = true; isl.crossed = true; isl.holders.forEach(n => istarByNode[n] = 'provisional'); }
      isl.parentHash = b.hash;
    }
    // heals: an island whose builders are back loses to the spine
    const live = new Set(activeGroups(slot).map(g => g.key));
    for (const isl of islands) {
      if (isl.closed || live.has(isl.key)) continue;
      isl.closed = true;
      isl.holders.forEach(v => { delete strandedAt[v]; });
      if (!isl.seg) continue; // never proposed: fell behind, caught up, no branch
      const dropped = isl.seg.blocks;
      isl.seg.state = 'orphaned'; isl.seg.holders = []; isl.seg.lost_by = isl.holders.slice();
      for (const v of isl.holders) {
        reorgs.push({ slot, node: v, depth: dropped, added: number - isl.ancestorNumber,
          ancestor: isl.ancestorHash, ancestor_number: isl.ancestorNumber, old_head: isl.seg.tip, new_head: prevHash,
          lost_segment: isl.seg.id, won_segment: 's0', crosses_istar: isl.seg.first_slot <= T && slot >= T });
        alerts.push({ slot, kind: 'warn', node: v, expected: true, detail: `canonical split healed: node ${v} rewound ${dropped} blocks to #${isl.ancestorNumber}` });
      }
      blocks.filter(b => b.segment === isl.seg.id && b.istar).forEach(b => { b.istar_orphaned = true; });
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
    const isl = islands.find(i => i.holders.includes(n) && !i.closed);
    const seg = isl && isl.seg ? isl.seg : spineSeg;
    let headBlock = blocks.filter(b => b.segment === seg.id).slice(-1)[0];
    if (isl && !isl.seg) {
      // Cut off with nothing proposed yet: still holding the block it was
      // stranded on, falling further behind every slot.
      headBlock = blocks.find(b => b.hash === strandedAt[n].hash) || headBlock;
    }
    headBlock = headBlock || blocks[0];
    const phase = nowSlot < T ? (n === 3 && nowSlot < 20 ? 'following' : 'synced') : (nowSlot < T + 60 ? 'window' : 'done');
    const lag = nowSlot < T ? (n === 3 ? 3 : 0) : 0;
    return {
      id: n, name: `el-${n}-${CLIENTS[n - 1]}-lighthouse`, client: CLIENTS[n - 1], head: headBlock.hash, head_number: headBlock.number, head_slot: headBlock.slot,
      segment: isl && !isl.seg ? spineSeg.id : seg.id,
      phase, cursor_number: headBlock.number - lag, cursor_hash: headBlock.hash, lag, cursor_detached: false,
      // A light only reports I* once its own chain crossed it: during the
      // straddle the islands cross at different slots, and only the anchor's
      // fork block finalizing makes it final for everyone.
      istar: !istarByNode[n] ? 'none' : (finalized >= T ? 'final' : 'provisional'),
      el_peers: isolated ? 0 : 3, cl_peers: isolated ? 0 : 3, finalized_slot: isolated ? Math.max(0, (isl && isl.seg ? isl.seg.first_slot : nowSlot) - 5) : finalized,
      status: nowSlot > 180 && nowSlot < 195 && n === 4 ? 'unreachable' : 'ok', isolated,
    };
  });
  for (const s of segments) if (s.state === 'live' && s.id !== 's0') s.holders = nodeViews.filter(v => v.segment === s.id).map(v => v.id);

  // The client table, with every cell state scripted somewhere in the lap so
  // a review can see them all without an enclave: besu never reports a shadow
  // root (noinspect), node 4 goes unreachable at 200, node 2 falls behind at
  // 168, an unexpected shadow dissent at 150-160 and an unexpected header
  // dissent at 330-345, plus whichever nodes are partitioned (expected).
  const compare = (() => {
    const introspects = { 1: true, 2: true, 3: true, 4: false }; // besu has no migration RPC
    const eligible = nodeViews.filter(v => v.status === 'ok' && !v.isolated && v.phase !== 'stalled');
    const empty = { height: 0, ref: '', ref_source: 'none', hash_agree: 0, hash_judged: 0, shadow_agree: 0, shadow_judged: 0, shadow_classes: 0, rows: [] };
    if (!eligible.length) return empty;
    const height = Math.max(0, Math.min(...eligible.map(v => v.head_number)) - 2);
    const refBlock = blocks.filter(b => b.segment === 's0' && b.number <= height).slice(-1)[0];
    if (!refBlock) return empty;
    const refShadow = hex(refBlock.format === 'mpt' ? 'pbt-shadow' : 'mpt-shadow', height);
    const refHeader = hex('hdr', height);

    const rows = nodeViews.map(v => {
      const r = {
        node: v.id, ahead: v.head_number - height, hash: refBlock.hash, header_root: refHeader,
        shadow_root: introspects[v.id] ? refShadow : '', expected: false, why: '',
        hash_state: 'agree', header_state: 'agree',
        shadow_state: introspects[v.id] ? 'agree' : 'noinspect',
        istar_state: v.istar === 'none' ? 'unjudged' : 'agree', verdict: 'ok',
      };
      if (v.status !== 'ok') {
        Object.assign(r, { hash: '', header_root: '', shadow_root: '', hash_state: 'unreachable', header_state: 'unreachable', shadow_state: 'unreachable', verdict: 'absent' });
      } else if (v.isolated) {
        Object.assign(r, {
          hash: hex('island', height + v.id), header_root: hex('island-hdr', height + v.id),
          shadow_root: introspects[v.id] ? hex('island-shadow', height + v.id) : '',
          hash_state: 'dissent', header_state: 'unjudged', shadow_state: introspects[v.id] ? 'unjudged' : 'noinspect',
          verdict: 'fork', expected: true, why: 'isolated',
        });
      } else if (v.id === 2 && nowSlot >= 168 && nowSlot < 176) {
        Object.assign(r, { ahead: -2, hash: '', header_root: '', shadow_root: '', hash_state: 'behind', header_state: 'behind', shadow_state: 'behind', verdict: 'absent' });
      } else if (v.id === 2 && nowSlot >= 200 && nowSlot < 212) {
        Object.assign(r, { shadow_root: hex('wrong-shadow', height), shadow_state: 'dissent', verdict: 'shadow-dissent' });
      } else if (v.id === 3 && nowSlot >= 330 && nowSlot < 345) {
        Object.assign(r, { header_root: hex('wrong-hdr', height), header_state: 'dissent', verdict: 'header-dissent' });
      } else if (introspects[v.id] && nowSlot % 17 === 0) {
        Object.assign(r, { shadow_root: '', shadow_state: 'pending', verdict: 'partial' }); // the asker has not got there yet
      } else if (!introspects[v.id]) {
        r.verdict = 'partial';
      }
      return r;
    });
    const judged = (k) => rows.filter(r => r[k] === 'agree' || r[k] === 'dissent');
    const shadows = new Set(rows.filter(r => r.shadow_state === 'agree' || r.shadow_state === 'dissent').map(r => r.shadow_root));
    return {
      height, ref: refBlock.hash, ref_source: 'anchor',
      hash_agree: rows.filter(r => r.hash_state === 'agree').length, hash_judged: judged('hash_state').length,
      shadow_agree: rows.filter(r => r.shadow_state === 'agree').length, shadow_judged: judged('shadow_state').length,
      shadow_classes: shadows.size, rows,
    };
  })();

  return {
    seq: nowSlot, now_slot: nowSlot, slot_seconds: SLOT_SECONDS, fork_slot: T, finalized_slot: finalized,
    nodes: nodeViews, segments, blocks, reorgs, partitions, schedule, alerts, compare,
  };
}
