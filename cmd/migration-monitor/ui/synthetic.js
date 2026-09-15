// Synthetic lap: a scripted six-node migration run that exercises every
// glyph the page draws, including the offline-migration lifecycle (`make
// ui-preview`). Deterministic (seeded), so a review compares the same
// picture; produces the same snapshot shape /api/state will serve.
//
// Timeline (slots, 6s each): genesis 0; offline lap first (node 6 exports
// 2-8, node 2 exports live 4-10, node 5 swaps node 6's artifacts 8-28, node
// 3 swaps node 2's artifacts serially after 16-48; ?variant=fail replaces
// node 5's swap with a terminal failure and node 3's with a stuck-reconnect
// timeout). Then the original four-node chaos drama on nodes 1-4: deep
// partitions on node 2 (40-72, 90-122); short on node 3 (132-157); node 4
// unreachable 200-215; straddle on node 3 across I*=300 (285-315: two I*
// candidates, node 3's orphaned); window partition on node 4 (340-365);
// post-fork deep and two scenarios. One injected shadow-root split at block
// #150 (node 4 dissents). Nodes 5/6 never fork: they ride the spine, their
// chip driven by the offline importer/producer step instead.

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
  const variant = (typeof location !== 'undefined' && new URLSearchParams(location.search).get('variant')) || 'happy';

  const T = 300, FINALITY_LAG = 30, SLOT_SECONDS = 6;
  const stake = { 1: 0.2, 2: 0.4, 3: 0.2, 4: 0.2 };
  const CHAOS_NODES = [1, 2, 3, 4]; // only these fork/partition; 5/6 ride the spine
  const ALL_NODES = [1, 2, 3, 4, 5, 6];
  // The migration profile's own layout, so the preview shows the marks the live page does.
  // Node 6: non-validating producer (geth-convert). Node 5: nethermind, consumer of node 6.
  const CLIENTS = ['geth', 'erigon', 'geth', 'besu', 'nethermind', 'geth'];

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
  const spineSeg = { id: 's0', lane: 0, ancestor: null, first_slot: 0, last_slot: 0, blocks: 0, tip: null, state: 'live', holders: ALL_NODES.slice(), lost_by: [] };
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
    const majorityStake = CHAOS_NODES.filter(n => !victims.includes(n)).reduce((a, n) => a + stake[n], 0);
    const format = slot >= T ? 'pbt' : 'mpt';

    if (rnd() < majorityStake) {
      number++;
      const b = mint(slot, spineSeg, prevHash, number, format);
      if (!crossedT && slot >= T) { b.istar = true; crossedT = true; CHAOS_NODES.filter(n => !victims.includes(n)).forEach(n => istarByNode[n] = 'provisional'); }
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

  // Offline-migration lifecycle for nodes 2/6 (producers) and 5/3 (consumers,
  // swapped serially, in config order). step_since is back-computed from the
  // simulated elapsed-in-step, not real wall time, so the client-side "fresh
  // for 3s" check on caught_up lines up with the scrubbed slot, not the clock.
  const spineBlockAt = (slot) => {
    let best = null;
    for (const b of blocks) if (b.segment === 's0' && b.slot <= slot && (!best || b.number > best.number)) best = b;
    return best;
  };
  const since = (startSlot) => Math.floor(Date.now() / 1000) - Math.max(0, nowSlot - startSlot) * SLOT_SECONDS;
  const anchor6Block = nowSlot >= 8 ? spineBlockAt(8) : null;
  const anchor2Block = nowSlot >= 10 ? spineBlockAt(10) : null;
  const anchor6 = anchor6Block ? anchor6Block.number : 0;
  const anchor2 = anchor2Block ? anchor2Block.number : 0;
  const producerStep = (fromSlot, exportedSlot, anchor) => {
    if (nowSlot < fromSlot) return { producer: true, producer_step: '' };
    if (nowSlot < exportedSlot) return { producer: true, producer_step: 'exporting', step_since: since(fromSlot) };
    return { producer: true, producer_step: 'exported', step_since: since(exportedSlot), anchor };
  };
  const onlineOnly = { 1: 'protected anchor: stays on the online path', 2: 'erigon has no cross-client importer', 4: 'besu has no importer' };

  // One consumer's disconnecting->importing->reconnecting->replaying->caught_up
  // ladder, parameterised by its start slot and which failure variant (if
  // any) it shows instead of settling.
  const swapSteps = (start, fail) => {
    const at = (importer, from, step_detail = '') => ({ importer, step_since: since(from), step_detail });
    if (nowSlot < start) return { importer: 'pending', step_since: 0 };
    if (nowSlot < start + 3) return at('disconnecting', start);
    if (nowSlot < start + 8) return at('importing', start + 3);
    if (fail === 'failed') return at('failed', start + 8, 'import exited: native PBT database holds no state');
    if (nowSlot < start + 10) return at('reconnecting', start + 8);
    if (fail === 'timeout') return at('timeout', start + 10, 'reconnect exceeded timeout_seconds=900');
    if (nowSlot < start + 18) return at('replaying', start + 10);
    return at('caught_up', start + 18);
  };
  const s5 = swapSteps(8, variant === 'fail' && 'failed');
  const s3 = swapSteps(variant === 'fail' ? 16 : 26, variant === 'fail' && 'timeout'); // node 3's swap queues behind node 5's

  const anchors = [];
  if (anchor6Block) anchors.push({ node: 6, number: anchor6, hash: anchor6Block.hash });
  if (anchor2Block) anchors.push({ node: 2, number: anchor2, hash: anchor2Block.hash });
  const swaps = [
    { node: 5, producer: 6, importer: s5.importer, anchor: anchor6, step_since: s5.step_since },
    { node: 3, producer: 2, importer: s3.importer, anchor: anchor2, step_since: s3.step_since },
  ];
  const offByNode = {
    1: { importer: 'no_importer', importer_reason: onlineOnly[1] },
    2: { ...producerStep(4, 10, anchor2), importer: 'no_importer', importer_reason: onlineOnly[2] },
    3: { ...s3, anchor: anchor2 },
    4: { importer: 'no_importer', importer_reason: onlineOnly[4] },
    5: { ...s5, anchor: anchor6 },
    6: producerStep(2, 8, anchor6),
  };

  const finalized = Math.max(0, nowSlot - FINALITY_LAG);
  const nodeViews = ALL_NODES.map(n => {
    const isolated = activeVictims(nowSlot).includes(n);
    const isl = islands.find(i => i.victim === n && !i.closed);
    const seg = isl ? isl.seg : spineSeg;
    const headBlock = blocks.filter(b => b.segment === seg.id).slice(-1)[0] || blocks[0];
    const phase = nowSlot < T ? (n === 3 && nowSlot < 20 ? 'following' : 'synced') : (nowSlot < T + 60 ? 'window' : 'done');
    const lag = nowSlot < T ? (n === 3 ? 3 : 0) : 0;
    return {
      id: n, name: `el-${n}-${CLIENTS[n - 1]}-lighthouse`, client: CLIENTS[n - 1], head: headBlock.hash, head_number: headBlock.number, head_slot: headBlock.slot, segment: seg.id,
      phase, cursor_number: headBlock.number - lag, cursor_hash: headBlock.hash, lag, cursor_detached: false,
      istar: nowSlot < T ? 'none' : (finalized >= T ? 'final' : (istarByNode[n] || 'provisional')),
      el_peers: isolated ? 0 : 3, cl_peers: isolated ? 0 : 3, finalized_slot: isolated ? Math.max(0, seg.first_slot - 5) : finalized,
      status: nowSlot > 200 && nowSlot < 215 && n === 4 ? 'unreachable' : 'ok', isolated,
      ...offByNode[n],
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
      } else if (v.id === 2 && nowSlot >= 150 && nowSlot < 160) {
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
    nodes: nodeViews, segments, blocks, reorgs, partitions, schedule, alerts, anchors, swaps, compare,
  };
}
