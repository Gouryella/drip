'use strict';

/* Drip patch panel. A tunnel is a port: allocated or blank, lit or dark. */

// ---- transport -----------------------------------------------------------

function csrfToken() {
  const match = document.cookie.match(/(?:^|;\s*)drip_admin_csrf=([^;]*)/);
  return match ? decodeURIComponent(match[1]) : '';
}

async function api(method, path, body) {
  const opts = { method, headers: { Accept: 'application/json' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  // Every state-changing verb echoes the CSRF cookie into a header. An
  // off-origin page cannot read that cookie, so it cannot forge the header.
  if (method !== 'GET') opts.headers['X-CSRF-Token'] = csrfToken();

  const res = await fetch(path, opts);
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch (_) { data = null; } }

  if (!res.ok) {
    const err = new Error(serverError((data && data.error) || res.statusText || t('common.requestFailed')));
    err.status = res.status;
    throw err;
  }
  return data;
}

// ---- dom -----------------------------------------------------------------

function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') node.className = v;
      else if (k === 'text') node.textContent = v;
      else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v === true ? '' : v);
    }
  }
  for (const child of children || []) if (child) node.appendChild(child);
  return node;
}

function svgIcon(id) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#' + id);
  svg.setAttribute('aria-hidden', 'true');
  svg.appendChild(use);
  return svg;
}

let statusTimer = null;
function status(message, bad) {
  const node = document.getElementById('status');
  node.textContent = message;
  node.classList.toggle('bad', !!bad);
  node.hidden = false;
  clearTimeout(statusTimer);
  statusTimer = setTimeout(() => { node.hidden = true; }, bad ? 9000 : 4500);
}

function bytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i ? 1 : 0)} ${units[i]}`;
}

function when(value) {
  if (!value) return t('common.never');
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? t('common.never') : d.toLocaleString(currentLang());
}

function pad(n) { return String(n).padStart(2, '0'); }

// ---- state ---------------------------------------------------------------

const state = {
  user: null,
  view: 'field',
  server: null,
  accounts: [],
  clients: [],
  reservations: [],
  linked: new Set(),   // subdomains lit at last render, for the energize moment
  seenLink: false,
  needsSetup: false,
};

function accountOf(id) { return state.accounts.find(a => a.id === id) || null; }
function accountLabel(id) { const a = accountOf(id); return a ? a.name : (id || t('common.none')); }
function clientLabel(id) {
  if (!id) return t('common.anyClient');
  const c = state.clients.find(x => x.id === id);
  return c ? c.name : id;
}
function tagClass(accountId) {
  const index = state.accounts.findIndex(a => a.id === accountId);
  return 'tag tag-' + (index < 0 ? 5 : (index % 8) + 1);
}
function canEdit() { return state.user && state.user.role === 'admin'; }

// multiTenant reports whether account columns and tags distinguish anything.
function multiTenant() { return state.accounts.length > 1; }

// cols drops the entries whose `when` is false, keeping headers and cells aligned.
function cols(entries) { return entries.filter(e => e.when !== false).map(e => e.value); }

async function loadLookups() {
  const [server, accounts, clients, reservations] = await Promise.all([
    state.server ? Promise.resolve(state.server) : api('GET', '/api/server'),
    api('GET', '/api/accounts'),
    api('GET', '/api/clients'),
    api('GET', '/api/reservations'),
  ]);
  state.server = server || null;
  state.accounts = accounts || [];
  state.clients = clients || [];
  state.reservations = reservations || [];
}

// ---- what a machine actually needs -------------------------------------

// tunnelURL mirrors the server's own URL builder: the port is omitted at 443.
function tunnelURL(subdomain) {
  const s = state.server;
  if (!s || !s.tunnel_domain || !subdomain) return '';
  const host = `${subdomain}.${s.tunnel_domain}`;
  return s.public_port === 443 ? `https://${host}` : `https://${host}:${s.public_port}`;
}

function tcpURL(port) {
  const s = state.server;
  if (!s || !s.tunnel_domain || !port) return '';
  return `tcp://${s.tunnel_domain}:${port}`;
}

// allocationURL is the address an allocation resolves to once a machine binds it.
function allocationURL(reservation) {
  return reservation.subdomain
    ? tunnelURL(reservation.subdomain)
    : tcpURL(reservation.tcp_port);
}

// connectCommand is the line an operator pastes on the machine being connected.
// The token only exists at issue time, so elsewhere it stands in as a placeholder.
function connectCommand(token, localPort) {
  const s = state.server;
  const server = s && s.domain
    ? `${s.domain}:${s.public_port || 443}`
    : 'your-server:443';
  return `drip http ${localPort || 8080} --server ${server} --token ${token}`;
}

// reservationFor finds the allocation a credential is bound to, if any.
function reservationFor(clientId) {
  return (state.reservations || []).find(r => r.client_id === clientId) || null;
}

// copyable renders a monospace strip with a copy button beside it.
function copyable(value, label) {
  const button = el('button', { type: 'button', class: 'btn-quiet small', text: t('common.copy') });
  button.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(value);
      button.textContent = t('common.copied');
      setTimeout(() => { button.textContent = t('common.copy'); }, 1600);
    } catch (_) {
      status(t('common.copyFailed'), true);
    }
  });
  return el('div', { class: 'copyable' }, [
    label ? el('span', { class: 'legend', text: label }) : null,
    el('div', { class: 'copyable-row' }, [
      el('code', { class: 'tag-strip', text: value }),
      button,
    ]),
  ]);
}

// ---- shared pieces -------------------------------------------------------

function head(title, note) {
  return el('div', { class: 'view-head' }, [
    el('h1', { text: title }),
    el('p', { class: 'note', text: note }),
  ]);
}

function panel(railChildren, bodyChildren) {
  const parts = [];
  if (railChildren) parts.push(el('div', { class: 'panel-rail' }, railChildren));
  if (bodyChildren) parts.push(el('div', { class: 'panel-body' }, bodyChildren));
  return el('div', { class: 'panel' }, parts);
}

function dataTable(headers, rows, emptyTitle, emptyHint) {
  if (!rows.length) {
    return el('div', { class: 'blank' }, [
      el('span', { class: 'legend', text: emptyTitle }),
      el('p', { class: 'note', text: emptyHint }),
    ]);
  }
  return el('div', { class: 'scroll' }, [
    el('table', null, [
      el('thead', null, [el('tr', null, headers.map(h => {
        const spec = typeof h === 'string' ? { label: h } : h;
        return el('th', { class: spec.right ? 'right' : null, text: spec.label || '' });
      }))]),
      el('tbody', null, rows),
    ]),
  ]);
}

function stateCell(kind, label) {
  return el('span', { class: 'state ' + kind }, [
    el('i', { class: 'led' }),
    el('span', { text: label }),
  ]);
}

// A destructive action swaps itself for an inline confirmation. No modal, and
// no browser dialog that would block the whole session.
function danger(label, confirmText, run) {
  const button = el('button', { type: 'button', class: 'btn-danger small', text: label });

  button.addEventListener('click', () => {
    // Replace the whole action group, not just this button: a confirmation
    // squeezed in beside the others overflows the column.
    const group = button.closest('.btn-row') || button;
    const cancel = el('button', { type: 'button', class: 'btn-quiet small', text: t('common.keep') });
    const go = el('button', { type: 'button', class: 'btn-danger small', text: label });
    const strip = el('span', { class: 'confirm' }, [
      el('span', { text: confirmText }), cancel, go,
    ]);

    const restore = () => { strip.replaceWith(group); button.focus(); };
    cancel.addEventListener('click', restore);
    go.addEventListener('click', async () => {
      go.disabled = true;
      cancel.disabled = true;
      go.textContent = t('common.working');
      try { await run(); } catch (err) { status(err.message, true); restore(); }
    });

    group.replaceWith(strip);
    cancel.focus();
  });

  return button;
}

// accountField returns the account control for a form, or null when there is
// nothing to decide. With a single account the id rides along as a hidden
// input, so a one-tenant deployment never sees the concept at all.
function accountField(name) {
  if (state.accounts.length === 1) {
    return el('input', { type: 'hidden', name, value: state.accounts[0].id });
  }
  const sel = el('select', { name });
  for (const a of state.accounts) sel.appendChild(el('option', { value: a.id, text: a.name }));
  return labelled(t('col.account'), sel);
}

// hasAccount reports whether any account exists to attach things to.
function hasAccount() { return state.accounts.length > 0; }

// needsAccountFirst is the blank state shown when nothing can be created yet.
function needsAccountFirst(kind) {
  return el('div', { class: 'blank' }, [
    el('span', { class: 'legend', text: t('common.noAccountYet') }),
    el('p', { class: 'note', text: t('common.noAccountHint' + kind) }),
  ]);
}

function clientSelect(name) {
  const sel = el('select', { name });
  sel.appendChild(el('option', { value: '', text: t('common.anyMachine') }));
  for (const c of state.clients) sel.appendChild(el('option', { value: c.id, text: c.name }));
  return sel;
}

function submitting(form, on) {
  for (const control of form.elements) control.disabled = on;
  const button = form.querySelector('button[type=submit]');
  if (button) button.textContent = on ? t('common.working') : button.dataset.label;
}

function bench(fields, submitLabel, handler) {
  const button = el('button', { type: 'submit', class: 'btn', text: submitLabel });
  button.dataset.label = submitLabel;
  const form = el('form', { class: 'bench' }, [...fields, button]);

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    // Read the values before disabling anything: FormData skips disabled
    // controls, so locking the form first would submit an empty body.
    const data = new FormData(form);
    submitting(form, true);
    try {
      await handler(data);
      form.reset();
      await render();
    } catch (err) {
      status(err.message, true);
      submitting(form, false);
    }
  });
  return form;
}

function labelled(text, control, grow) {
  return el('label', { class: 'field' + (grow ? ' grow' : '') }, [
    el('span', { class: 'legend', text }), control,
  ]);
}

// ---- field ---------------------------------------------------------------

function portKey(r) { return r.subdomain || ('tcp-' + r.tcp_port); }

async function viewField(root) {
  const reservations = state.reservations;
  const tunnels = await api('GET', '/api/tunnels');

  const live = new Map();
  for (const t of tunnels || []) live.set(t.subdomain, t);

  // Every allocation is a port. A live tunnel nobody allocated is an
  // unlabelled patch, and gets a port at the end of the field.
  const ports = (reservations || []).map(r => ({
    key: portKey(r),
    label: r.subdomain || `tcp ${r.tcp_port}`,
    accountId: r.account_id,
    clientId: r.client_id,
    enabled: r.enabled,
    tunnel: live.get(portKey(r)) || null,
    allocated: true,
  }));

  for (const t of tunnels || []) {
    if (!ports.some(p => p.key === t.subdomain)) {
      ports.push({
        key: t.subdomain,
        label: t.subdomain,
        accountId: t.account_id,
        clientId: t.client_id,
        enabled: true,
        tunnel: t,
        allocated: false,
      });
    }
  }

  const linkedNow = new Set(ports.filter(p => p.tunnel).map(p => p.key));
  const linkedCount = linkedNow.size;
  const darkCount = ports.filter(p => p.allocated && !p.tunnel).length;

  root.appendChild(head(t('field.title'), t('field.note')));

  const rail = [
    el('div', { class: 'counts' }, [
      el('span', { class: 'count linked' }, [
        el('b', { text: String(linkedCount) }), el('span', { class: 'legend', text: t('field.linked') }),
      ]),
      el('span', { class: 'count' }, [
        el('b', { text: String(darkCount) }), el('span', { class: 'legend', text: t('field.dark') }),
      ]),
      el('span', { class: 'count' }, [
        el('b', { text: String(ports.length) }), el('span', { class: 'legend', text: t('field.ports') }),
      ]),
    ]),
    el('button', {
      type: 'button', class: 'btn-quiet small', text: t('field.reread'),
      onclick: () => render(),
    }),
  ];

  if (!ports.length) {
    root.appendChild(panel(rail, [
      el('div', { class: 'blank' }, [
        el('span', { class: 'legend', text: t('field.empty') }),
        el('p', { class: 'note', text: t('field.emptyHint') }),
      ]),
    ]));
    return;
  }

  const grid = el('div', { class: 'field-grid' });
  ports.forEach((p, i) => {
    const linked = !!p.tunnel;
    const fresh = linked && state.seenLink && !state.linked.has(p.key);

    let cls = 'port';
    if (linked) cls += ' is-linked';
    if (fresh) cls += ' just-linked';
    if (!p.enabled) cls += ' is-disabled is-held';

    const stripClass = p.allocated ? 'port-strip' : 'port-strip unlabelled';
    const detail = linked
      ? `${p.tunnel.active_connections} ${t('field.conn')} · ${bytes(p.tunnel.bytes_in + p.tunnel.bytes_out)}`
      : (p.allocated
        ? (p.enabled ? t('field.stateDark') : t('field.stateDisabled'))
        : t('field.stateUnlabelled'));

    grid.appendChild(el('div', {
      class: cls,
      title: `${p.label} — ${accountLabel(p.accountId)}`,
    }, [
      el('span', { class: 'port-no', text: pad(i + 1) }),
      el('span', { class: 'port-jack' }, [svgIcon('keystone'), el('i', { class: 'led' })]),
      el('span', { class: stripClass, text: p.label }),
      el('span', { class: 'port-meta' }, [
        el('i', { class: tagClass(p.accountId) }),
        el('span', { class: 'legend', text: detail }),
      ]),
    ]));
  });

  state.linked = linkedNow;
  state.seenLink = true;

  // A rack panel is never left half-populated; unused positions carry blanking
  // plates. Recomputed on reflow so the row stays full at every width.
  const fillBlanks = () => {
    for (const blank of grid.querySelectorAll('.port-blank')) blank.remove();
    const columns = getComputedStyle(grid).gridTemplateColumns.split(' ').filter(Boolean).length;
    if (!columns) return;
    const remainder = ports.length % columns;
    if (!remainder) return;
    for (let i = remainder; i < columns; i++) {
      grid.appendChild(el('div', { class: 'port port-blank', 'aria-hidden': 'true' }));
    }
  };

  root.appendChild(panel(rail, [grid]));
  fillBlanks();
  if (window.ResizeObserver) {
    const observer = new ResizeObserver(fillBlanks);
    observer.observe(grid);
  }

  // What is actually flowing. Only linked ports appear here; the field above
  // already answers "is it up", this answers "what is it doing".
  const linkedPorts = ports.filter(p => p.tunnel);
  if (linkedPorts.length) {
    const rows = linkedPorts.map(p => el('tr', null, cols([
      { value: el('td', null, [el('span', { class: 'state' }, [
        el('i', { class: tagClass(p.accountId) }),
        el('span', { class: 'mono', text: p.label }),
      ])]) },
      { value: el('td', { text: p.tunnel.tunnel_type }) },
      { value: el('td', { text: p.clientId ? clientLabel(p.clientId) : t('field.unauthenticated') }) },
      { value: el('td', { text: accountLabel(p.accountId) }), when: multiTenant() },
      { value: el('td', { class: 'right', text: String(p.tunnel.active_connections) }) },
      { value: el('td', { class: 'right', text: bytes(p.tunnel.bytes_in) }) },
      { value: el('td', { class: 'right', text: bytes(p.tunnel.bytes_out) }) },
      { value: el('td', { text: when(p.tunnel.last_active) }) },
    ])));

    root.appendChild(panel(
      [el('span', { class: 'legend', text: t('field.detail') })],
      [dataTable(
        cols([
          { value: t('col.port') }, { value: t('col.type') }, { value: t('col.machine') },
          { value: t('col.account'), when: multiTenant() },
          { value: { label: t('col.conns'), right: true } },
          { value: { label: t('col.in'), right: true } },
          { value: { label: t('col.out'), right: true } },
          { value: t('col.lastActive') },
        ]),
        rows, '', '',
      )],
    ));
  }

  // Which colour tags which account, printed on the panel like a legend card.
  if (multiTenant()) {
    root.appendChild(panel(
      [
        el('span', { class: 'legend', text: t('field.tagLegend') }),
        el('div', { class: 'counts legend-row' }, state.accounts.map(a =>
          el('span', { class: 'count' }, [
            el('i', { class: tagClass(a.id) }),
            el('span', { class: 'legend', text: a.name }),
          ]))),
      ],
      null,
    ));
  }
}

// ---- allocations ---------------------------------------------------------

async function viewReservations(root) {
  const list = state.reservations;

  root.appendChild(head(t('alloc.title'), t('alloc.note')));

  if (canEdit() && !hasAccount()) {
    root.appendChild(panel(null, [needsAccountFirst('Allocation')]));
  } else if (canEdit()) {
    root.appendChild(panel(
      [el('span', { class: 'legend', text: t('alloc.reserveHead') })],
      [bench([
        accountField('account_id'),
        labelled(t('col.machine'), clientSelect('client_id')),
        labelled(t('alloc.subdomain'), el('input', { name: 'subdomain', placeholder: 'billing', autocapitalize: 'off', spellcheck: 'false' }), true),
        labelled(t('alloc.tcpPort'), el('input', { name: 'tcp_port', type: 'number', min: '1', max: '65535', placeholder: '20050' })),
        labelled(t('col.bandwidth'), el('input', { name: 'bandwidth', placeholder: '1M' })),
      ], t('alloc.submit'), async (data) => {
        const port = parseInt(data.get('tcp_port'), 10);
        await api('POST', '/api/reservations', {
          account_id: data.get('account_id'),
          client_id: data.get('client_id') || null,
          subdomain: (data.get('subdomain') || '').trim(),
          tcp_port: Number.isNaN(port) ? 0 : port,
          bandwidth: (data.get('bandwidth') || '').trim(),
        });
        status(t('alloc.reserved'));
      })],
    ));
  }

  const rows = list.map(r => el('tr', null, cols([
    { value: el('td', null, [el('span', { class: 'state' }, [
      el('i', { class: tagClass(r.account_id) }),
      el('span', { class: 'mono', text: allocationURL(r) || r.subdomain || `tcp ${r.tcp_port}` }),
    ])]) },
    { value: el('td', { text: r.tunnel_type }) },
    { value: el('td', { text: accountLabel(r.account_id) }), when: multiTenant() },
    { value: el('td', { text: clientLabel(r.client_id) }) },
    { value: el('td', { text: r.bandwidth || t('common.none') }) },
    { value: el('td', null, [r.enabled
      ? stateCell('on', t('common.enabled'))
      : stateCell('held', t('common.disabled'))]) },
    { value: el('td', { class: 'right' }, [canEdit() ? el('span', { class: 'btn-row' }, [
      el('button', {
        type: 'button', class: 'btn-quiet small', text: r.enabled ? t('common.disable') : t('common.enable'),
        onclick: async (e) => {
          e.target.disabled = true;
          try {
            await api('PATCH', `/api/reservations/${r.id}`, { enabled: !r.enabled });
            await render();
          } catch (err) { status(err.message, true); e.target.disabled = false; }
        },
      }),
      danger(t('alloc.release'), t('alloc.releaseConfirm'), async () => {
        await api('DELETE', `/api/reservations/${r.id}`);
        status(t('alloc.released'));
        await render();
      }),
    ]) : null]) },
  ])));

  root.appendChild(panel(
    [el('span', { class: 'legend', text: t('alloc.count', { n: list.length }) })],
    [dataTable(
      cols([
        { value: t('col.port') }, { value: t('col.type') },
        { value: t('col.account'), when: multiTenant() },
        { value: t('col.machine') }, { value: t('col.bandwidth') }, { value: t('col.state') },
        { value: { label: '', right: true } },
      ]),
      rows,
      t('alloc.empty'),
      t('alloc.emptyHint'),
    )],
  ));
}

// ---- credentials ---------------------------------------------------------

async function viewClients(root) {
  root.appendChild(head(t('client.title'), t('client.note')));

  if (canEdit() && !hasAccount()) {
    root.appendChild(panel(null, [needsAccountFirst('Credential')]));
  } else if (canEdit()) {
    root.appendChild(panel(
      [el('span', { class: 'legend', text: t('client.issueHead') })],
      [bench([
        accountField('account_id'),
        labelled(t('client.name'), el('input', { name: 'name', placeholder: 'win-svc-01', required: true, autocapitalize: 'off', spellcheck: 'false' }), true),
        labelled(t('col.bandwidth'), el('input', { name: 'bandwidth', placeholder: '1M' })),
      ], t('client.submit'), async (data) => {
        const created = await api('POST', '/api/clients', {
          account_id: data.get('account_id'),
          name: (data.get('name') || '').trim(),
          bandwidth: (data.get('bandwidth') || '').trim(),
        });
        showToken(created.token, created);
      })],
    ));
  }

  const rows = state.clients.map(c => el('tr', null, cols([
    { value: el('td', null, [el('span', { class: 'state' }, [
      el('i', { class: tagClass(c.account_id) }),
      el('span', { text: c.name }),
    ])]) },
    { value: el('td', null, [(() => {
      const reservation = reservationFor(c.id);
      return reservation
        ? el('span', { class: 'mono', text: allocationURL(reservation) })
        : el('span', { class: 'legend', text: t('client.randomName') });
    })()]) },
    { value: el('td', { text: accountLabel(c.account_id) }), when: multiTenant() },
    { value: el('td', { text: c.bandwidth || t('common.none') }) },
    { value: el('td', { text: when(c.last_seen_at) }) },
    { value: el('td', null, [c.enabled
      ? stateCell('on', t('common.enabled'))
      : stateCell('held', t('common.disabled'))]) },
    { value: el('td', { class: 'right' }, [canEdit() ? el('span', { class: 'btn-row' }, [
      el('button', {
        type: 'button', class: 'btn-quiet small', text: c.enabled ? t('common.disable') : t('common.enable'),
        onclick: async (e) => {
          e.target.disabled = true;
          try {
            await api('PATCH', `/api/clients/${c.id}`, { enabled: !c.enabled });
            await render();
          } catch (err) { status(err.message, true); e.target.disabled = false; }
        },
      }),
      danger(t('client.rotate'), t('client.rotateConfirm'), async () => {
        const out = await api('POST', `/api/clients/${c.id}/rotate`);
        showToken(out.token, c);
        await render();
      }),
      danger(t('common.delete'), t('client.deleteConfirm'), async () => {
        await api('DELETE', `/api/clients/${c.id}`);
        status(t('client.deleted'));
        await render();
      }),
    ]) : null]) },
  ])));

  root.appendChild(panel(
    [el('span', { class: 'legend', text: t('client.count', { n: state.clients.length }) })],
    [dataTable(
      cols([
        { value: t('col.machine') }, { value: t('col.address') },
        { value: t('col.account'), when: multiTenant() },
        { value: t('col.bandwidth') }, { value: t('col.lastSeen') }, { value: t('col.state') },
        { value: { label: '', right: true } },
      ]),
      rows,
      t('client.empty'),
      t('client.emptyHint'),
    )],
  ));
}

// ---- accounts ------------------------------------------------------------

async function viewAccounts(root) {
  root.appendChild(head(t('account.title'), t('account.note')));

  if (canEdit()) {
    root.appendChild(panel(
      [el('span', { class: 'legend', text: t('account.addHead') })],
      [bench([
        labelled(t('account.name'), el('input', { name: 'name', placeholder: 'acme', required: true, autocapitalize: 'off' }), true),
        labelled(t('account.ceiling'), el('input', { name: 'max_tunnels', type: 'number', min: '0', value: '0' })),
      ], t('account.submit'), async (data) => {
        await api('POST', '/api/accounts', {
          name: (data.get('name') || '').trim(),
          max_tunnels: parseInt(data.get('max_tunnels'), 10) || 0,
        });
        status(t('account.added'));
      })],
    ));
  }

  const rows = state.accounts.map(a => el('tr', null, [
    el('td', null, [el('span', { class: 'state' }, [
      el('i', { class: tagClass(a.id) }), el('span', { text: a.name }),
    ])]),
    el('td', { class: 'mono', text: a.id }),
    el('td', { text: a.max_tunnels ? String(a.max_tunnels) : t('account.noCeiling') }),
    el('td', null, [a.enabled
      ? stateCell('on', t('common.enabled'))
      : stateCell('held', t('common.disabled'))]),
    el('td', { class: 'right' }, [canEdit() ? el('span', { class: 'btn-row' }, [
      el('button', {
        type: 'button', class: 'btn-quiet small', text: a.enabled ? t('common.disable') : t('common.enable'),
        onclick: async (e) => {
          e.target.disabled = true;
          try {
            await api('PATCH', `/api/accounts/${a.id}`, { enabled: !a.enabled });
            await render();
          } catch (err) { status(err.message, true); e.target.disabled = false; }
        },
      }),
      danger(t('common.delete'), t('account.deleteConfirm'), async () => {
        await api('DELETE', `/api/accounts/${a.id}`);
        status(t('account.deleted'));
        await render();
      }),
    ]) : null]),
  ]));

  root.appendChild(panel(
    [el('span', { class: 'legend', text: t('account.count', { n: state.accounts.length }) })],
    [dataTable(
      [t('col.account'), t('col.id'), t('col.ceiling'), t('col.state'), { label: '', right: true }],
      rows,
      t('account.empty'),
      t('account.emptyHint'),
    )],
  ));
}

// ---- log -----------------------------------------------------------------

async function viewAudit(root) {
  const entries = await api('GET', '/api/audit?limit=200') || [];

  root.appendChild(head(t('audit.title'), t('audit.note')));

  const rows = entries.map(e => el('tr', null, [
    el('td', { text: when(e.at) }),
    el('td', { text: e.actor_id || e.actor_type }),
    el('td', { class: 'mono', text: e.action }),
    el('td', { class: 'wrap', text: e.detail || e.target_id || t('common.none') }),
    el('td', { class: 'mono', text: e.ip || t('common.none') }),
  ]));

  root.appendChild(panel(
    [el('span', { class: 'legend', text: t('audit.count', { n: entries.length }) })],
    [dataTable(
      [t('col.when'), t('col.who'), t('col.action'), t('col.target'), t('col.from')],
      rows,
      t('audit.empty'),
      t('audit.emptyHint'),
    )],
  ));
}

// ---- token reveal --------------------------------------------------------

const tokenDialog = document.getElementById('token-dialog');

function showToken(token, client) {
  const body = document.getElementById('token-body');
  body.textContent = '';

  const reservation = client ? reservationFor(client.id) : null;
  const url = reservation ? allocationURL(reservation) : '';

  body.appendChild(copyable(connectCommand(token, 8080), t('token.command')));
  body.appendChild(copyable(token, t('token.alone')));

  if (url) {
    body.appendChild(el('p', { class: 'note' }, [
      document.createTextNode(t('token.reachablePre')),
      el('strong', { text: url }),
      document.createTextNode(t('token.reachablePost')),
    ]));
  } else {
    body.appendChild(el('p', { class: 'note', text: t('token.noReservation') }));
  }

  tokenDialog.returnValue = '';
  tokenDialog.showModal();
}

document.getElementById('token-done').addEventListener('click', () => {
  tokenDialog.close();
  document.getElementById('token-body').textContent = '';
});

// The token cannot be recovered, so dismissing by Escape must be deliberate too.
tokenDialog.addEventListener('cancel', (e) => {
  e.preventDefault();
  status(t('token.copyFirst'), true);
});

// ---- shell ---------------------------------------------------------------

const views = {
  field: viewField,
  reservations: viewReservations,
  clients: viewClients,
  accounts: viewAccounts,
  audit: viewAudit,
};

function skeleton(root) {
  const grid = el('div', { class: 'field-grid' });
  for (let i = 0; i < 8; i++) {
    grid.appendChild(el('div', { class: 'port skeleton-port' }, [
      el('div', { class: 'skeleton', style: 'width:40%' }),
      el('div', { class: 'skeleton', style: 'width:80%' }),
      el('div', { class: 'skeleton', style: 'width:60%' }),
    ]));
  }
  root.appendChild(el('div', { class: 'panel' }, [el('div', { class: 'panel-body' }, [grid])]));
}

let rendering = false;
async function render() {
  if (rendering) return;
  rendering = true;

  const root = document.getElementById('view');
  root.textContent = '';
  skeleton(root);

  try {
    await loadLookups();
    root.textContent = '';
    await views[state.view](root);
  } catch (err) {
    root.textContent = '';
    if (err.status === 401) { rendering = false; return showGate(false); }
    root.appendChild(el('div', { class: 'blank' }, [
      el('span', { class: 'legend', text: t('common.readFailed') }),
      el('p', { class: 'note', text: err.message }),
      el('p', null, [el('button', { type: 'button', class: 'btn-quiet', text: t('common.tryAgain'), onclick: () => render() })]),
    ]));
  } finally {
    rendering = false;
  }
}

// roleLabel translates the known roles and leaves an unknown one as the server
// spelled it, rather than showing a lookup key.
function roleLabel(role) {
  const label = t('role.' + role);
  return label === 'role.' + role ? role : label;
}

// gateText writes the gate copy for the current language. Kept apart from
// showGate so switching language mid-sign-in does not steal the focus.
function gateText() {
  document.getElementById('gate-title').textContent = state.needsSetup ? t('gate.setup') : t('gate.signin');
  document.getElementById('gate-hint').textContent = state.needsSetup ? t('gate.setupHint') : '';
  document.getElementById('gate-form').dataset.setup = state.needsSetup ? '1' : '';
  const submit = document.querySelector('#gate-form button[type=submit]');
  submit.dataset.label = t('gate.continue');
  if (!submit.disabled) submit.textContent = submit.dataset.label;
}

function showGate(needsSetup) {
  document.getElementById('app').hidden = true;
  const gate = document.getElementById('gate');
  gate.hidden = false;

  state.needsSetup = !!needsSetup;
  gateText();
  gate.querySelector('input[name=username]').focus();
}

function whoami() {
  document.getElementById('whoami').textContent = `${state.user.username} · ${roleLabel(state.user.role)}`;
}

function showApp() {
  document.getElementById('gate').hidden = true;
  document.getElementById('app').hidden = false;
  whoami();
  render();
}

// Switching language re-renders whatever is on screen: the static markup is
// handled by applyStatic, everything else was built by these views.
function languageChanged() {
  gateText();
  if (!document.getElementById('app').hidden && state.user) {
    whoami();
    render();
  }
}

document.getElementById('gate-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const form = e.target;
  const errBox = document.getElementById('gate-error');
  errBox.hidden = true;

  // Captured before the form is locked; FormData omits disabled controls.
  const data = new FormData(form);
  submitting(form, true);

  const path = form.dataset.setup ? '/api/bootstrap' : '/api/session';

  try {
    state.user = await api('POST', path, {
      username: data.get('username'),
      password: data.get('password'),
    });
    form.reset();
    submitting(form, false);
    showApp();
  } catch (err) {
    errBox.textContent = err.message;
    errBox.hidden = false;
    submitting(form, false);
  }
});

document.querySelector('#gate-form button[type=submit]').dataset.label = t('gate.continue');

document.getElementById('signout').addEventListener('click', async () => {
  try { await api('DELETE', '/api/session'); } catch (_) { /* sign out regardless */ }
  state.user = null;
  state.linked = new Set();
  state.seenLink = false;
  showGate(false);
});

document.getElementById('tabs').addEventListener('click', (e) => {
  const button = e.target.closest('button[data-view]');
  if (!button) return;
  state.view = button.dataset.view;
  for (const b of document.querySelectorAll('#tabs button')) {
    b.classList.toggle('active', b === button);
    b.setAttribute('aria-current', b === button ? 'page' : 'false');
  }
  render();
});

applyStatic();
document.getElementById('lang-app').appendChild(languageSelect(languageChanged));
document.getElementById('lang-gate').appendChild(languageSelect(languageChanged));

(async function boot() {
  try {
    state.user = await api('GET', '/api/session');
    showApp();
  } catch (_) {
    try {
      const setup = await api('GET', '/api/bootstrap');
      showGate(!!(setup && setup.needs_setup));
    } catch (_err) {
      showGate(false);
    }
  }
})();
