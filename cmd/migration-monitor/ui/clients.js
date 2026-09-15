// The client table: every participant's answer at ONE height, side by side.
//
// Two rules the colours obey, because a table that cries wolf gets ignored:
// agreement is never coloured (silence is the good case, counted in the
// caption), and a disagreement the devnet asked for - an isolated victim, a
// node still settling after a heal - is amber with its reason, not red. Red
// means only: nobody asked for this.
//
// Rows are keyed by node and mutated in place. Everything else on this page is
// destroyed and rebuilt each frame, which is fine for SVG but would eat text
// selection and scroll position in a table you are trying to read roots from.

const PHASE = {
  deep: '#b04ad6', synced: '#2fa84f', parked: '#8d8d8d', window: '#ffc27a',
  done: '#ff8a1f', stalled: '#e0433d', opaque: '#5a5a5a', unknown: '#c8c8c8',
};

// What the client's own migration RPC says it is doing. Before I* the binary
// direction decides the phase, after I* the merkle one, which is why the same
// word means a different tree on each side of the fork (feeds.go:179-240).
const PHASE_DOC = {
  following: 'Before I*: building the binary tree in the background, still behind the head.',
  synced: 'Before I*: the binary tree is built and keeping up with the head.',
  parked: 'Nothing is being built - the deciding direction is idle.',
  window: 'After I*: the binary tree is now the primary root AND the merkle tree is still kept as a shadow. The retirement window is open, which is the only time the two can be cross-checked.',
  done: 'Retired: the client dropped the other tree, which geth does once its fork block finalizes.',
  stalled: 'The migration direction reported an error and stopped.',
  opaque: 'This client has no migration RPC at all, so it has no phase to report. Not the same as "unknown".',
  unknown: 'The client stopped answering, so nothing is known about its phase.',
};

const COLS = [
  ['client', 'which implementation, and the participant number it runs as'],
  ['phase', 'what the client migration RPC says it is doing: before I* the binary direction decides, after I* the merkle one. Hover a cell for that phase.'],
  ['head', 'its head, and how far that is from the compared height'],
  ['hash', 'its canonical block hash at the compared height'],
  ['header root', 'the root in that block header - the primary root (MPT before I*, PBT after)'],
  ['shadow root', 'the other format, from the client migration RPC; "–" = this client has none'],
  ['I*', 'the fork block this client crossed on'],
  ['peers', 'EL / CL peer counts'],
  ['verdict', 'the worst thing true of this row'],
];

// Verdict -> the bar on the row's left edge. `exception` splits by whether the
// devnet asked for the disagreement.
const ROW_BAR = {
  ok: 'row-ok',
  partial: 'row-partial',
  absent: 'row-absent',
  fork: 'exception',
  'header-dissent': 'exception',
  'shadow-dissent': 'exception',
  'istar-dissent': 'exception',
};

// State -> cell rendering. Values absent for a reason read as that reason;
// only a real disagreement gets a fill.
const MARK = { pending: '?', noinspect: '–', unreachable: '⊘' };

export function renderClients(host, s) {
  const c = s.compare || { rows: [], ref_source: 'none' };
  const byNode = new Map(c.rows.map(r => [r.node, r]));
  const table = ensure(host, 'table', 'table', () => {
    const t = document.createElement('table');
    t.id = 'clients';
    const thead = t.createTHead().insertRow();
    for (const [name, doc] of COLS) {
      const th = document.createElement('th');
      th.textContent = name; th.title = doc; thead.appendChild(th);
    }
    t.createTBody();
    return t;
  });
  const body = table.tBodies[0];

  const seen = new Set();
  for (const n of s.nodes) {
    seen.add(n.id);
    const row = byNode.get(n.id) || {};
    const tr = rowFor(body, n.id);
    paintRow(tr, n, row, c);
  }
  for (const tr of [...body.rows]) {
    if (!seen.has(+tr.dataset.node)) tr.remove();
  }

  const cap = ensure(host, 'cap', 'div', () => {
    const d = document.createElement('div'); d.className = 'cap'; return d;
  });
  setText(cap, caption(c));
  cap.title = c.ref_source === 'anchor'
    ? 'compared against the anchor, which is never partitioned and holds the canonical chain'
    : c.ref_source === 'majority'
      ? 'the anchor did not answer; compared against a strict majority of the settled nodes'
      : 'no reference this tick - nothing is judged rather than guessing one';
}

function caption(c) {
  if (c.ref_source === 'none' || !c.rows.length) return 'no comparison this tick · no node is both settled and answering';
  const parts = [
    `height ${c.height}`,
    `ref ${c.ref_source} ${short(c.ref)}`,
    `hash ${c.hash_agree}/${c.hash_judged}`,
  ];
  if (c.shadow_judged) {
    parts.push(`shadow ${c.shadow_agree}/${c.shadow_judged}` + (c.shadow_classes > 1 ? ` · ${c.shadow_classes} classes` : ''));
  }
  return parts.join(' · ');
}

function rowFor(body, node) {
  let tr = body.querySelector(`tr[data-node="${node}"]`);
  if (tr) return tr;
  tr = body.insertRow();
  tr.dataset.node = node;
  for (let i = 0; i < COLS.length; i++) tr.insertCell();
  // The client mark, reusing the symbols the chips already define.
  const mark = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  mark.setAttribute('class', 'mark'); mark.setAttribute('viewBox', '0 0 24 24');
  mark.appendChild(document.createElementNS('http://www.w3.org/2000/svg', 'use'));
  tr.cells[0].appendChild(mark);
  tr.cells[0].appendChild(document.createElement('span'));
  return tr;
}

function paintRow(tr, n, r, c) {
  const [name, phase, head, hash, header, shadow, istar, peers, verdict] = tr.cells;

  const use = name.querySelector('use');
  const href = `#logo-${n.client}`;
  if (use.getAttribute('href') !== href) use.setAttribute('href', href);
  setText(name.querySelector('span'), `${n.id} · ${n.client}`);
  name.title = n.name;

  setText(phase, n.phase + (n.isolated ? ' · cut off' : ''));
  phase.style.setProperty('--dot', PHASE[n.phase] || '#c8c8c8');
  setClass(phase, 'phase');
  phase.title = (PHASE_DOC[n.phase] || '') + (n.isolated ? '\nCut off right now: this node is a partition victim.' : '');

  const ahead = r.ahead === undefined ? '' : r.ahead === 0 ? 'at' : r.ahead > 0 ? `+${r.ahead}` : `${r.ahead}`;
  setText(head, n.head_number ? `#${n.head_number} ${ahead}` : '–');
  setClass(head, 'mono');

  cell(hash, r.hash, r.hash_state, r);
  cell(header, r.header_root, r.header_state, r);
  cell(shadow, r.shadow_root, r.shadow_state, r);
  cell(istar, n.istar === 'none' ? '' : n.istar, r.istar_state, r);
  if (n.istar !== 'none') istar.title = n.istar === 'final' ? 'crossed, and its fork block is finalized' : 'crossed, not yet finalized';

  setText(peers, `${n.el_peers}/${n.cl_peers < 0 ? '–' : n.cl_peers}`);
  setClass(peers, 'mono muted');

  setText(verdict, r.verdict === 'ok' ? '' : (r.verdict || ''));
  setClass(verdict, r.verdict && r.verdict !== 'ok' && r.verdict !== 'partial' ? (r.expected ? 'warn' : 'bad') : 'muted');
  verdict.title = r.why || '';

  // The bar on the left states the row at a glance: green nothing wrong, pale
  // green nothing wrong but nothing cross-checkable either, grey no answer,
  // amber a disagreement the devnet asked for, red one nobody asked for.
  setClass(tr, ROW_BAR[r.verdict] === undefined ? ''
    : ROW_BAR[r.verdict] === 'exception' ? (r.expected ? 'row-warn' : 'row-bad')
      : ROW_BAR[r.verdict]);
}

function cell(td, value, state, r) {
  const text = state === 'behind' ? `v${Math.abs(r.ahead || 0)}` : (MARK[state] ?? short(value));
  setText(td, text);
  let cls = 'mono';
  if (state === 'dissent') cls += r.expected ? ' warn' : ' bad';
  else if (state === 'agree') cls += ' agreed';
  else cls += ' muted';
  setClass(td, cls);
  td.title = state === 'dissent' && r.why ? `${value} · expected: ${r.why}`
    : state === 'noinspect' ? 'this client has no migration RPC; absence is its contract, not a fault'
      : state === 'unjudged' ? 'not comparable: this node is not on the reference block'
        : (value || '');
}

function short(v) {
  if (!v) return '';
  return v.length > 16 ? `${v.slice(0, 10)}…${v.slice(-4)}` : v;
}

function ensure(host, key, tag, make) {
  let el = host.querySelector(`[data-slot="${key}"]`);
  if (!el) {
    el = make();
    el.dataset.slot = key;
    host.appendChild(el);
  }
  return el;
}

function setText(el, text) { if (el.textContent !== text) el.textContent = text; }
function setClass(el, cls) { if (el.className !== cls) el.className = cls; }
