'use strict';

/* Panel copy in two languages. The dictionary is the only place English text
   lives; every view asks for a key, never a literal. */

const LOCALES = [
  { code: 'en', label: 'English' },
  { code: 'pt-BR', label: 'Português (Brasil)' },
];

const STRINGS = {
  en: {
    'app.title': 'Drip · patch panel',
    'app.legend': 'patch panel',
    'app.sections': 'Sections',
    'app.language': 'Language',
    'app.signout': 'Sign out',

    'tab.field': 'Field',
    'tab.reservations': 'Allocations',
    'tab.clients': 'Credentials',
    'tab.accounts': 'Accounts',
    'tab.audit': 'Log',

    'gate.legend': 'Drip control plane',
    'gate.signin': 'Sign in',
    'gate.setup': 'Create the first administrator',
    'gate.setupHint': 'This deployment has no administrator yet. Choose credentials — the password must be at least 12 characters.',
    'gate.username': 'Username',
    'gate.password': 'Password',
    'gate.continue': 'Continue',

    'token.legend': 'Issued once',
    'token.title': 'Credential token',
    'token.note': 'This is the only time the token is shown. It cannot be recovered — rotating the credential is the only way to get a new one.',
    'token.done': 'I have saved it',
    'token.command': 'Run this on the machine',
    'token.alone': 'Token on its own',
    'token.reachablePre': 'Once connected, this machine is reachable at ',
    'token.reachablePost': '. It binds the same address on every reconnect.',
    'token.noReservation': 'This machine has no reserved name yet, so it will get a random one each time it connects. Reserve a port for it under Allocations to pin it.',
    'token.copyFirst': 'Copy the token before closing — it is not shown again.',

    'common.copy': 'Copy',
    'common.copied': 'Copied',
    'common.copyFailed': 'Could not reach the clipboard. Select the text and copy it manually.',
    'common.working': 'Working',
    'common.keep': 'Keep',
    'common.enable': 'Enable',
    'common.disable': 'Disable',
    'common.delete': 'Delete',
    'common.enabled': 'enabled',
    'common.disabled': 'disabled',
    'common.never': 'never',
    'common.none': '—',
    'common.requestFailed': 'request failed',
    'common.tryAgain': 'Try again',
    'common.readFailed': 'Could not read the control plane',
    'common.anyMachine': 'any machine on the account',
    'common.anyClient': 'any client on the account',
    'common.noAccountYet': 'No account yet',
    'common.noAccountHintAllocation': 'Every allocation belongs to an account. Add one under Accounts first.',
    'common.noAccountHintCredential': 'Every credential belongs to an account. Add one under Accounts first.',

    'field.title': 'Field',
    'field.note': 'Every allocation is a port. A lit indicator means a client is connected to it right now.',
    'field.linked': 'linked',
    'field.dark': 'allocated, dark',
    'field.ports': 'ports',
    'field.reread': 'Re-read',
    'field.empty': 'Panel empty',
    'field.emptyHint': 'No allocations and nothing connected. Create an account, issue a credential, then reserve a name for it — the reserved name is what the client binds to on every reconnect.',
    'field.stateDark': 'dark',
    'field.stateDisabled': 'disabled',
    'field.stateUnlabelled': 'unlabelled',
    'field.conn': 'conn',
    'field.detail': 'Linked detail',
    'field.tagLegend': 'Tag legend',
    'field.unauthenticated': 'unauthenticated',

    'col.port': 'Port',
    'col.type': 'Type',
    'col.machine': 'Machine',
    'col.account': 'Account',
    'col.conns': 'Conns',
    'col.in': 'In',
    'col.out': 'Out',
    'col.lastActive': 'Last active',
    'col.bandwidth': 'Bandwidth',
    'col.state': 'State',
    'col.address': 'Address',
    'col.lastSeen': 'Last seen',
    'col.id': 'ID',
    'col.ceiling': 'Tunnel ceiling',
    'col.when': 'When',
    'col.who': 'Who',
    'col.action': 'Action',
    'col.target': 'Target',
    'col.from': 'From',

    'alloc.title': 'Allocations',
    'alloc.note': 'A reserved subdomain or TCP port. Bind it to a machine and that machine lands on the same URL every time it reconnects; leave it unbound and any machine may claim it by asking for the name.',
    'alloc.reserveHead': 'Reserve a port',
    'alloc.subdomain': 'Subdomain',
    'alloc.tcpPort': 'or TCP port',
    'alloc.submit': 'Reserve',
    'alloc.reserved': 'Port reserved.',
    'alloc.release': 'Release',
    'alloc.releaseConfirm': 'Release this name?',
    'alloc.released': 'Allocation released.',
    'alloc.count': '{n} allocated',
    'alloc.count.one': '1 allocated',
    'alloc.empty': 'Nothing allocated',
    'alloc.emptyHint': 'Reserve a subdomain and every reconnect from that client lands on the same URL.',

    'client.title': 'Credentials',
    'client.note': 'One credential per machine. The machine presents it to connect; the token is shown once, when it is issued or rotated.',
    'client.issueHead': 'Issue a credential',
    'client.name': 'Machine name',
    'client.submit': 'Issue',
    'client.randomName': 'random name each connect',
    'client.rotate': 'Rotate',
    'client.rotateConfirm': 'Rotate? The old token dies now.',
    'client.deleteConfirm': 'Delete? Allocations become unbound.',
    'client.deleted': 'Credential deleted.',
    'client.count': '{n} issued',
    'client.count.one': '1 issued',
    'client.empty': 'No credentials issued',
    'client.emptyHint': 'A credential is what a machine presents to connect. Issue one per machine, then reserve a port for it.',

    'account.title': 'Accounts',
    'account.note': 'An account groups the machines and names belonging to one tenant, and caps how many tunnels they may open at once. Running this server for yourself? Keep the single default account and ignore this page — you only need more when separate customers or teams share the deployment.',
    'account.addHead': 'Add an account',
    'account.name': 'Name',
    'account.ceiling': 'Tunnel ceiling (0 = none)',
    'account.submit': 'Add',
    'account.added': 'Account added.',
    'account.noCeiling': 'no ceiling',
    'account.deleteConfirm': 'Delete with all its credentials and allocations?',
    'account.deleted': 'Account deleted.',
    'account.count': '{n} accounts',
    'account.count.one': '1 account',
    'account.empty': 'No accounts',
    'account.emptyHint': 'An account is the owner every credential and allocation hangs off. Add one to begin.',

    'audit.title': 'Log',
    'audit.note': 'Administrative changes, most recent first.',
    'audit.count': '{n} entries',
    'audit.count.one': '1 entry',
    'audit.empty': 'Nothing logged yet',
    'audit.emptyHint': 'Every change made through this panel is recorded here with who made it and from where.',

    'role.admin': 'admin',
    'role.viewer': 'viewer',

    'err.invalid username or password': 'invalid username or password',
    'err.not signed in': 'not signed in',
    'err.no such endpoint': 'no such endpoint',
    'err.internal error': 'internal error',
    'err.this action requires the admin role': 'this action requires the admin role',
    'err.too many sign-in attempts; wait a few minutes': 'too many sign-in attempts; wait a few minutes',
    'err.an administrator already exists; sign in instead': 'an administrator already exists; sign in instead',
    'err.provide exactly one of subdomain or tcp_port': 'provide exactly one of subdomain or tcp_port',
    'err.CSRF token mismatch': 'CSRF token mismatch',
    'err.already exists': 'already exists',
    'err.not found': 'not found',
    'err.account name is required': 'account name is required',
    'err.client name is required': 'client name is required',
    'err.a subdomain is required for http reservations': 'a subdomain is required for http reservations',
  },

  'pt-BR': {
    'app.title': 'Drip · painel de conexões',
    'app.legend': 'painel de conexões',
    'app.sections': 'Seções',
    'app.language': 'Idioma',
    'app.signout': 'Sair',

    'tab.field': 'Campo',
    'tab.reservations': 'Alocações',
    'tab.clients': 'Credenciais',
    'tab.accounts': 'Contas',
    'tab.audit': 'Registro',

    'gate.legend': 'Plano de controle do Drip',
    'gate.signin': 'Entrar',
    'gate.setup': 'Criar o primeiro administrador',
    'gate.setupHint': 'Esta instalação ainda não tem administrador. Escolha as credenciais — a senha precisa ter pelo menos 12 caracteres.',
    'gate.username': 'Usuário',
    'gate.password': 'Senha',
    'gate.continue': 'Continuar',

    'token.legend': 'Exibido uma única vez',
    'token.title': 'Token da credencial',
    'token.note': 'Esta é a única vez que o token aparece. Ele não pode ser recuperado — girar a credencial é o único jeito de obter um novo.',
    'token.done': 'Guardei o token',
    'token.command': 'Execute isto na máquina',
    'token.alone': 'Somente o token',
    'token.reachablePre': 'Depois de conectada, esta máquina responde em ',
    'token.reachablePost': '. Ela assume o mesmo endereço a cada reconexão.',
    'token.noReservation': 'Esta máquina ainda não tem nome reservado, então recebe um aleatório a cada conexão. Reserve uma porta para ela em Alocações para fixá-la.',
    'token.copyFirst': 'Copie o token antes de fechar — ele não é exibido de novo.',

    'common.copy': 'Copiar',
    'common.copied': 'Copiado',
    'common.copyFailed': 'Não foi possível acessar a área de transferência. Selecione o texto e copie manualmente.',
    'common.working': 'Processando',
    'common.keep': 'Manter',
    'common.enable': 'Ativar',
    'common.disable': 'Desativar',
    'common.delete': 'Excluir',
    'common.enabled': 'ativa',
    'common.disabled': 'desativada',
    'common.never': 'nunca',
    'common.none': '—',
    'common.requestFailed': 'a requisição falhou',
    'common.tryAgain': 'Tentar de novo',
    'common.readFailed': 'Não foi possível ler o plano de controle',
    'common.anyMachine': 'qualquer máquina da conta',
    'common.anyClient': 'qualquer cliente da conta',
    'common.noAccountYet': 'Nenhuma conta ainda',
    'common.noAccountHintAllocation': 'Toda alocação pertence a uma conta. Crie uma em Contas primeiro.',
    'common.noAccountHintCredential': 'Toda credencial pertence a uma conta. Crie uma em Contas primeiro.',

    'field.title': 'Campo',
    'field.note': 'Cada alocação é uma porta. Indicador aceso significa que há um cliente conectado nela agora.',
    'field.linked': 'com link',
    'field.dark': 'alocadas, apagadas',
    'field.ports': 'portas',
    'field.reread': 'Reler',
    'field.empty': 'Painel vazio',
    'field.emptyHint': 'Nenhuma alocação e nada conectado. Crie uma conta, emita uma credencial e reserve um nome para ela — o nome reservado é o que o cliente assume a cada reconexão.',
    'field.stateDark': 'apagada',
    'field.stateDisabled': 'desativada',
    'field.stateUnlabelled': 'sem etiqueta',
    'field.conn': 'conex.',
    'field.detail': 'Detalhe do link',
    'field.tagLegend': 'Legenda das cores',
    'field.unauthenticated': 'sem autenticação',

    'col.port': 'Porta',
    'col.type': 'Tipo',
    'col.machine': 'Máquina',
    'col.account': 'Conta',
    'col.conns': 'Conex.',
    'col.in': 'Entrada',
    'col.out': 'Saída',
    'col.lastActive': 'Última atividade',
    'col.bandwidth': 'Banda',
    'col.state': 'Estado',
    'col.address': 'Endereço',
    'col.lastSeen': 'Visto por último',
    'col.id': 'ID',
    'col.ceiling': 'Teto de túneis',
    'col.when': 'Quando',
    'col.who': 'Quem',
    'col.action': 'Ação',
    'col.target': 'Alvo',
    'col.from': 'De',

    'alloc.title': 'Alocações',
    'alloc.note': 'Um subdomínio ou porta TCP reservado. Vincule a uma máquina e ela cai na mesma URL toda vez que reconectar; deixe sem vínculo e qualquer máquina pode assumi-lo pedindo o nome.',
    'alloc.reserveHead': 'Reservar uma porta',
    'alloc.subdomain': 'Subdomínio',
    'alloc.tcpPort': 'ou porta TCP',
    'alloc.submit': 'Reservar',
    'alloc.reserved': 'Porta reservada.',
    'alloc.release': 'Liberar',
    'alloc.releaseConfirm': 'Liberar este nome?',
    'alloc.released': 'Alocação liberada.',
    'alloc.count': '{n} alocadas',
    'alloc.count.one': '1 alocada',
    'alloc.empty': 'Nada alocado',
    'alloc.emptyHint': 'Reserve um subdomínio e toda reconexão daquele cliente cai na mesma URL.',

    'client.title': 'Credenciais',
    'client.note': 'Uma credencial por máquina. A máquina a apresenta para conectar; o token aparece uma única vez, quando é emitido ou girado.',
    'client.issueHead': 'Emitir uma credencial',
    'client.name': 'Nome da máquina',
    'client.submit': 'Emitir',
    'client.randomName': 'nome aleatório a cada conexão',
    'client.rotate': 'Girar',
    'client.rotateConfirm': 'Girar? O token antigo morre agora.',
    'client.deleteConfirm': 'Excluir? As alocações ficam sem vínculo.',
    'client.deleted': 'Credencial excluída.',
    'client.count': '{n} emitidas',
    'client.count.one': '1 emitida',
    'client.empty': 'Nenhuma credencial emitida',
    'client.emptyHint': 'A credencial é o que a máquina apresenta para conectar. Emita uma por máquina e depois reserve uma porta para ela.',

    'account.title': 'Contas',
    'account.note': 'Uma conta agrupa as máquinas e os nomes de um mesmo inquilino e limita quantos túneis podem ficar abertos ao mesmo tempo. Roda este servidor só para você? Fique com a conta padrão e ignore esta página — só precisa de mais quando clientes ou times distintos dividem a instalação.',
    'account.addHead': 'Criar uma conta',
    'account.name': 'Nome',
    'account.ceiling': 'Teto de túneis (0 = sem teto)',
    'account.submit': 'Criar',
    'account.added': 'Conta criada.',
    'account.noCeiling': 'sem teto',
    'account.deleteConfirm': 'Excluir junto com todas as credenciais e alocações?',
    'account.deleted': 'Conta excluída.',
    'account.count': '{n} contas',
    'account.count.one': '1 conta',
    'account.empty': 'Nenhuma conta',
    'account.emptyHint': 'A conta é a dona de toda credencial e alocação. Crie uma para começar.',

    'audit.title': 'Registro',
    'audit.note': 'Mudanças administrativas, da mais recente para a mais antiga.',
    'audit.count': '{n} entradas',
    'audit.count.one': '1 entrada',
    'audit.empty': 'Nada registrado ainda',
    'audit.emptyHint': 'Toda mudança feita por este painel é registrada aqui com quem fez e de onde.',

    'role.admin': 'administrador',
    'role.viewer': 'leitura',

    'err.invalid username or password': 'usuário ou senha inválidos',
    'err.not signed in': 'sessão não iniciada',
    'err.no such endpoint': 'endpoint inexistente',
    'err.internal error': 'erro interno',
    'err.this action requires the admin role': 'esta ação exige o papel de administrador',
    'err.too many sign-in attempts; wait a few minutes': 'tentativas de login demais; espere alguns minutos',
    'err.an administrator already exists; sign in instead': 'já existe um administrador; faça login',
    'err.provide exactly one of subdomain or tcp_port': 'informe exatamente um entre subdomínio e porta TCP',
    'err.CSRF token mismatch': 'token CSRF não confere',
    'err.already exists': 'já existe',
    'err.not found': 'não encontrado',
    'err.account name is required': 'o nome da conta é obrigatório',
    'err.client name is required': 'o nome da máquina é obrigatório',
    'err.a subdomain is required for http reservations': 'reservas http exigem um subdomínio',
  },
};

const LANG_KEY = 'drip_admin_lang';

// pickLang: an explicit choice wins, then the browser's preference, then English.
function pickLang() {
  let stored = null;
  try { stored = localStorage.getItem(LANG_KEY); } catch (_) { stored = null; }
  if (stored && STRINGS[stored]) return stored;

  for (const tag of navigator.languages || [navigator.language || '']) {
    const lower = String(tag).toLowerCase();
    if (lower === 'pt-br' || lower === 'pt' || lower.startsWith('pt-')) return 'pt-BR';
    if (lower.startsWith('en')) return 'en';
  }
  return 'en';
}

let lang = pickLang();

function currentLang() { return lang; }

// t looks the key up in the active language and falls back to English, then to
// the key itself so a missing string is visible rather than blank.
function t(key, vars) {
  const table = STRINGS[lang] || STRINGS.en;
  // A count of one takes the `.one` form where the language defines one, so
  // "1 allocated" never comes out as "1 allocations".
  const wanted = vars && Number(vars.n) === 1 && table[key + '.one'] !== undefined
    ? key + '.one'
    : key;
  let out = table[wanted];
  if (out === undefined) out = STRINGS.en[wanted];
  if (out === undefined) out = STRINGS.en[key];
  if (out === undefined) return key;
  if (vars) {
    for (const [name, value] of Object.entries(vars)) {
      out = out.split('{' + name + '}').join(String(value));
    }
  }
  return out;
}

// serverError translates the fixed messages the API returns. Anything else —
// wrapped store errors, mostly — passes through untouched rather than being
// swallowed by a lookup miss.
function serverError(message) {
  const key = 'err.' + message;
  const table = STRINGS[lang] || STRINGS.en;
  return table[key] !== undefined ? table[key] : message;
}

// applyStatic translates the markup that ships in index.html. Attributes are
// addressed as `data-i18n-attr="attr:key"`, text as `data-i18n="key"`.
function applyStatic(root) {
  const scope = root || document;
  for (const node of scope.querySelectorAll('[data-i18n]')) {
    node.textContent = t(node.dataset.i18n);
  }
  for (const node of scope.querySelectorAll('[data-i18n-attr]')) {
    for (const pair of node.dataset.i18nAttr.split(',')) {
      const [attr, key] = pair.split(':');
      if (attr && key) node.setAttribute(attr.trim(), t(key.trim()));
    }
  }
  document.documentElement.lang = lang;
  document.title = t('app.title');
}

// setLang persists the choice and hands control back to the caller, which owns
// re-rendering the parts of the panel that JavaScript built.
function setLang(next, onChanged) {
  if (!STRINGS[next] || next === lang) return;
  lang = next;
  try { localStorage.setItem(LANG_KEY, next); } catch (_) { /* private mode: session-only */ }
  applyStatic();
  if (onChanged) onChanged();
}

// languageSelect builds a picker bound to the shared language state. Several may
// exist at once (the gate has one, the header has another); each is refreshed
// through the registry so they never disagree.
const pickers = [];
function languageSelect(onChanged) {
  const sel = document.createElement('select');
  sel.className = 'lang';
  sel.setAttribute('aria-label', t('app.language'));
  for (const locale of LOCALES) {
    const option = document.createElement('option');
    option.value = locale.code;
    option.textContent = locale.label;
    if (locale.code === lang) option.selected = true;
    sel.appendChild(option);
  }
  sel.addEventListener('change', () => {
    setLang(sel.value, () => {
      for (const other of pickers) {
        other.value = lang;
        other.setAttribute('aria-label', t('app.language'));
      }
      if (onChanged) onChanged();
    });
  });
  pickers.push(sel);
  return sel;
}
