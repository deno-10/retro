'use strict';

// ---------- Хранилище браузера ----------

const basePath = document.querySelector('meta[name="retro-base-path"]')?.content || '';
const participant = document.querySelector('meta[name="retro-participant"]')?.content || '';
const appPath = path => basePath + path;

const corporateParticipant = document.querySelector('meta[name="retro-auth"]')?.content === 'baas' ? participant : '';
const NAME_KEY = 'retro.name' + (corporateParticipant ? ':' + corporateParticipant : '');
const ANON_KEY = 'retro.anonymous' + (corporateParticipant ? ':' + corporateParticipant : '');
const RECENT_KEY = 'retro.recent:' + participant;
const TIMER_KEY = 'retro.timer' + (corporateParticipant ? ':' + corporateParticipant : '');

function stored(key, fallback) {
  try { return localStorage.getItem(key) ?? fallback; } catch { return fallback; }
}
function store(key, value) {
  try { localStorage.setItem(key, value); } catch { /* приватный режим — переживём */ }
}

// crypto.randomUUID недоступен без HTTPS (кроме localhost), а во внутренних
// сетях http встречается часто. getRandomValues работает везде.
function randomId() {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('');
}

function authorName() {
  return stored(ANON_KEY, '0') === '1' ? '' : stored(NAME_KEY, '');
}

function recentBoards() {
  try { return JSON.parse(stored(RECENT_KEY, '[]')); } catch { return []; }
}
function rememberBoard(id, title) {
  const list = recentBoards().filter(b => b.id !== id);
  list.unshift({ id, title, at: Date.now() });
  store(RECENT_KEY, JSON.stringify(list.slice(0, 10)));
}

// ---------- Утилиты ----------

const PHASES = [
  { id: 'write', label: 'Запись', hint: 'Пишите карточки. Чужие карточки скрыты до показа.' },
  { id: 'reveal', label: 'Показ', hint: 'Все карточки открыты. Прочитайте и дополните.' },
  { id: 'vote', label: 'Голосование', hint: 'Отметьте самое важное — по одному голосу на карточку.' },
  { id: 'discuss', label: 'Обсуждение', hint: 'Карточки отсортированы по голосам. Обсудите сверху вниз.' },
];

// h('tag', {attrs}, ...children) — маленький конструктор DOM вместо innerHTML:
// текст всегда экранирован, обработчики вешаются напрямую.
function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'href' && typeof v === 'string' && v.startsWith('/') && !v.startsWith('//')) el.setAttribute(k, appPath(v));
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

async function api(method, path, body, options = {}) {
  const controller = new AbortController();
  let timedOut = false;
  let rejectAbort;
  const aborted = new Promise((_, reject) => { rejectAbort = reject; });
  const onAbort = () => rejectAbort(new DOMException('Запрос отменён', 'AbortError'));
  controller.signal.addEventListener('abort', onAbort, { once: true });
  const forwardAbort = () => controller.abort();
  options.signal?.addEventListener('abort', forwardAbort, { once: true });
  if (options.signal?.aborted) controller.abort();
  const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, 15000);
  try {
    const request = (async () => {
      const res = await fetch(appPath(path), {
        method, cache: 'no-store',
        headers: {
          'Content-Type': 'application/json', 'X-Participant': participant,
          ...(options.idempotencyKey ? { 'Idempotency-Key': options.idempotencyKey } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: controller.signal,
      });
      let data;
      try { data = await res.json(); }
      catch (err) { if (res.ok || controller.signal.aborted) throw err; }
      if (!res.ok) {
        const err = new Error(data?.error || `Ошибка ${res.status}`);
        err.status = res.status;
        err.unknownOutcome = method !== 'GET' && res.status >= 500;
        throw err;
      }
      return data;
    })();
    // Срок ожидания охватывает и чтение тела: заголовки могут прийти,
    // а JSON — зависнуть. Отмена также передаётся самому fetch.
    return await Promise.race([request, aborted]);
  } catch (err) {
    if (err.status) throw err;
    const failure = new Error(timedOut
      ? 'Сервер не ответил за 15 секунд. Результат запроса не подтверждён.'
      : 'Не удалось получить ответ сервера. Проверьте соединение.');
    failure.unknownOutcome = method !== 'GET';
    failure.timeout = timedOut;
    throw failure;
  } finally {
    clearTimeout(timeout);
    controller.signal.removeEventListener('abort', onAbort);
    options.signal?.removeEventListener('abort', forwardAbort);
  }
}

let toastTimer;
function toast(message) {
  const el = document.getElementById('toast');
  el.textContent = message;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 3500);
}

function dots(n) {
  return h('span', { class: 'dots', 'aria-hidden': 'true' },
    Array.from({ length: n }, () => h('i', { class: 'dot' })));
}

async function copyText(value, success) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(value);
    } else {
      const field = h('textarea', { value, readonly: true, class: 'clipboard-field' });
      document.body.append(field);
      try {
        field.select();
        if (!document.execCommand('copy')) throw new Error();
      } finally {
        field.remove();
      }
    }
    toast(success);
  } catch {
    window.prompt('Скопируйте значение', value);
  }
}

function copyParticipantButton() {
  return h('button', { type: 'button', onclick: () => copyText(participant, 'ID участника скопирован') }, 'Скопировать мой ID');
}

function showUnavailable(root) {
  document.title = 'Доска недоступна · Ретро';
  root.hidden = false;
  root.replaceChildren(h('main', { class: 'notfound' },
    h('h1', {}, 'Доска недоступна'),
    h('p', {}, 'Проверьте ссылку. Если она верная, отправьте ведущему свой ID участника. Когда он добавит вас на доску, обновите страницу.'),
    copyParticipantButton(),
    h('a', { href: '/' }, 'На главную')));
}

function brandLink(className = '') {
  return h('a', { href: '/', class: `brand-link ${className}`, 'aria-label': 'Авито · Ретро — на главную' },
    h('span', { class: 'brand-mark', 'aria-hidden': 'true' },
      Array.from({ length: 4 }, () => h('i'))),
    h('span', { class: 'brand-labels' },
      h('span', { class: 'brand-company' }, 'Авито'),
      h('span', { class: 'brand-title' },
        h('span', { class: 'brand-name' }, 'ретро'),
        h('span', { class: 'brand-team', 'aria-hidden': 'true' }, 'team'))));
}

// Перерисовка заменяет DOM целиком. Чтобы набираемый текст и фокус не
// пропадали, поля помечены data-keep: перед рендером запоминаем активное
// поле, после — возвращаем в него значение, фокус и курсор.
function captureFocus() {
  const el = document.activeElement;
  if (!el || !el.dataset || !el.dataset.keep) return null;
  return {
    keep: el.dataset.keep,
    value: el.value,
    start: el.selectionStart,
    end: el.selectionEnd,
    scrollTop: el.scrollTop,
  };
}
function restoreFocus(saved) {
  if (!saved) return;
  const el = document.querySelector(`[data-keep="${CSS.escape(saved.keep)}"]`);
  if (!el) return;
  if (el.value !== saved.value) el.value = saved.value;
  el.focus({ preventScroll: true });
  try {
    if (saved.start != null) el.setSelectionRange(saved.start, saved.end);
  } catch { /* number-поля не поддерживают выделение */ }
  el.scrollTop = saved.scrollTop;
}

// ---------- Главная ----------

async function renderHome(root) {
  document.title = 'Ретро';
  const nameInput = h('input', {
    type: 'text', maxlength: 60, autocomplete: 'off',
    placeholder: 'Как подписывать ваши карточки',
    value: stored(NAME_KEY, ''),
  });
  const titleInput = h('input', {
    type: 'text', maxlength: 120, autocomplete: 'off', autofocus: true,
    placeholder: 'Например: Ретро спринта 42',
  });
  const select = h('select', {});
  const submit = h('button', { type: 'submit', class: 'primary' }, 'Создать доску',
    h('span', { class: 'button-arrow', 'aria-hidden': 'true' }, '↗'));

  const form = h('form', {
    class: 'create', id: 'new-board',
    onsubmit: async (e) => {
      e.preventDefault();
      store(NAME_KEY, nameInput.value.trim());
      submit.disabled = true;
      try {
        const board = await api('POST', '/api/boards', { title: titleInput.value, template: select.value });
        rememberBoard(board.id, board.title);
        location.href = appPath('/b/' + board.id);
      } catch (err) {
        toast(err.message);
        submit.disabled = false;
      }
    },
  },
    h('div', { class: 'create-heading' },
      h('span', { class: 'form-eyebrow' }, 'Новая доска'),
      h('h2', {}, 'Начнём ретро'),
      h('p', { class: 'muted' }, 'Создайте доску и добавьте команду по ID участников.')),
    h('label', {}, 'Название', titleInput),
    h('label', {}, 'Шаблон', select),
    h('label', {}, 'Ваше имя', nameInput),
    submit,
    copyParticipantButton(),
    h('p', { class: 'form-note' }, 'Можно писать анонимно. Вы решаете, когда открыть карточки.'),
  );

  const hero = h('section', { class: 'hero' },
    h('span', { class: 'eyebrow' }, 'Ретроспективы для команды'),
    h('h1', {}, 'Становимся ', h('br'), 'лучше.', h('em', {}, 'Вместе.')),
    h('p', { class: 'lead' }, 'Говорим о том, что получилось. Находим, что изменить. Превращаем обратную связь в следующий шаг команды.'),
    h('div', { class: 'hero-art', 'aria-hidden': 'true' },
      h('img', { src: '/static/retro-shapes.svg', alt: '', width: 640, height: 280 }),
      h('span', { class: 'art-caption' }, 'Разные взгляды. Общий результат.'),
    ),
  );

  const steps = [
    ['Запишите мысли', 'Каждый — в своём темпе'],
    ['Откройте карточки', 'Посмотрите на спринт вместе'],
    ['Выберите важное', 'Проголосуйте за главные темы'],
    ['Договоритесь', 'Запишите решения и ответственных'],
  ];
  const page = h('main', { class: 'home-page' }, hero, form,
    h('section', { class: 'how-it-works', id: 'how-it-works', 'aria-labelledby': 'steps-title' },
      h('div', { class: 'section-heading' },
        h('h2', { id: 'steps-title' }, 'От мыслей ', h('br'), 'к действиям'),
        h('p', {}, 'Четыре шага, чтобы услышать друг друга и договориться о главном.')),
      h('ol', { class: 'features', 'aria-label': 'Как проходит ретро' }, steps.map(([title, text], i) =>
        h('li', {}, h('span', { class: 'feature-num', 'aria-hidden': 'true' }, `0${i + 1}`),
          h('div', {}, h('strong', {}, title), h('p', {}, text)))))));
  root.replaceChildren(
    h('header', { class: 'home-header' },
      h('div', { class: 'home-header-inner' },
        h('div', { class: 'home-brand' }, brandLink()),
        h('nav', { class: 'home-nav', 'aria-label': 'Главная' },
          h('a', { href: '#how-it-works' }, 'Как проходит ретро'),
          h('a', { href: '#new-board', class: 'header-cta' }, 'Новая доска',
            h('span', { class: 'button-arrow', 'aria-hidden': 'true' }, '↗'))))),
    page,
    h('footer', { class: 'home-footer' },
      h('span', {}, 'Меньше недосказанности.'), h('strong', {}, 'Больше команды.')),
  );
  // При возврате назад состав «Недавних» и ID должны принадлежать текущей сессии.
  window.addEventListener('pagehide', () => { root.hidden = true; });
  window.addEventListener('pageshow', event => { if (event.persisted) location.reload(); });

  const recent = recentBoards();
  if (recent.length) {
    page.append(h('section', { class: 'recent' },
      h('h2', {}, 'Недавние доски'),
      h('ul', {}, recent.map(b => h('li', {},
        h('a', { href: '/b/' + b.id }, b.title || 'Без названия'),
        h('span', { class: 'muted' }, new Date(b.at).toLocaleDateString('ru-RU')),
      ))),
    ));
  }

  try {
    const templates = await api('GET', '/api/templates');
    for (const t of templates) {
      select.append(h('option', { value: t.id }, `${t.name}: ${t.columns.join(' / ')}`));
    }
  } catch (err) {
    toast(err.message);
  }
}

// ---------- Доска ----------

function renderBoardPage(root, id) {
  root.hidden = true;
  let board = null;
  let online = 0;
  let editingId = null;
  let editingActionId = null;
  const drafts = Object.create(null);
  const actionDraft = { text: '', assignee: '' };
  const cardEdits = new Map();
  const actionEdits = new Map();
  const pendingCards = new Set();
  const pendingDone = new Map();
  const pendingVotes = new Map();
  const jiraSelected = new Map();
  const jiraChecking = new Set();
  let jiraBusy = false;
  let jiraNotice = '';
  let actionPending = false;
  let writingPending = false;
  let joiningRound = null;
  let joinFailedRound = null;
  let writingNotice = '';
  const unsent = [];
  let composing = false;
  let renderPending = false;
  let renderFrame = null;
  let disposed = false;
  let pageGeneration = 0;
  const requests = new Set();
  let eventSource = null;
  let polling = false;
  let pollTimer = null;
  let pollInFlight = false;
  let pollController = null;
  const draftStorageKey = `retro.drafts.v1:${id}:${participant}`;
  let lastSavedDrafts = null;
  let storageNotice = null;
  let rejectedDraftsRaw = null;
  let draftStorageUnreadable = false;
  let draftWriteFailed = false;
  let lastStoredRaw = null;
  let draftStorageDiverged = false;

  const titleEl = h('h1', { class: 'board-title' });
  const chainEl = h('nav', { class: 'chain', 'aria-label': 'Ретро этой команды' });
  const phaseNav = h('nav', { class: 'phases', 'aria-label': 'Фазы ретро' });
  const toolsEl = h('div', { class: 'tools' });
  const hintEl = h('p', { class: 'hint' });
  const timerEl = h('span', { class: 'timer', role: 'timer', hidden: true });
  const statusEl = h('span', { class: 'status', hidden: true });
  const writingEl = h('section', { class: 'writing-ready', 'aria-label': 'Готовность участников', hidden: true });
  const columnsEl = h('main', { class: 'columns' });
  const recoveryEl = h('section', { 'aria-label': 'Неотправленные записи' });
  const draftWarningEl = h('div', { class: 'draft-warning', role: 'alert', hidden: true });

  // Замена DOM посреди IME прерывает ввод. Дождёмся завершения композиции
  // и последнего input, затем покажем самое свежее состояние доски.
  root.addEventListener('compositionstart', () => { composing = true; });
  root.addEventListener('compositionend', () => {
    composing = false;
    if (renderPending) renderFrame = requestAnimationFrame(render);
  });

  root.replaceChildren(
    h('header', { class: 'topbar' },
      h('div', { class: 'topbar-left' }, brandLink('home'), titleEl),
      phaseNav,
      toolsEl,
    ),
    h('div', { class: 'statusline' }, hintEl, timerEl, statusEl, chainEl),
    writingEl,
    draftWarningEl,
    recoveryEl,
    columnsEl,
  );
  const boardStatus = h('p', { class: 'access-status', role: 'status' }, 'Проверяем доступ к доске…');
  root.before(boardStatus);
  restoreDrafts();
  renderRecovery();

  function setStatus(text) {
    statusEl.textContent = text;
    statusEl.hidden = !text;
  }

  function warnStorage(kind, message) {
    storageNotice = kind;
    draftWarningEl.textContent = message;
    if (kind === 'invalid') draftWarningEl.append(h('div', {}, h('button', {
      type: 'button',
      onclick: () => {
        if (!confirm('Удалить сохранённые данные, которые не удалось прочитать? Текущий текст в полях останется.')) return;
        try { sessionStorage.removeItem(draftStorageKey); }
        catch {
          warnStorage('invalid', 'Не удалось сбросить сохранённые данные. Новые черновики пока не записываются.');
          return;
        }
        rejectedDraftsRaw = null;
        lastSavedDrafts = null;
        lastStoredRaw = null;
        draftStorageDiverged = false;
        draftStorageUnreadable = false;
        draftWriteFailed = false;
        storageNotice = null;
        draftWarningEl.hidden = true;
        persistDrafts();
      },
    }, 'Сбросить сохранённые черновики')));
    if (kind === 'diverged') draftWarningEl.append(h('div', {}, h('button', {
      type: 'button',
      onclick: () => {
        if (!confirm('Заменить текущие черновики сохранённой версией? Несохранённый текст в полях и очереди будет потерян. Сначала скопируйте нужное.')) return;
        restoreDrafts(true);
        renderRecovery();
        // Удалённое активное поле не должно вернуть старое значение через
        // captureFocus после явного выбора другой версии черновиков.
        columnsEl.replaceChildren();
        render();
      },
    }, 'Загрузить сохранённые черновики')));
    draftWarningEl.hidden = false;
  }

  function draftSnapshot() {
    const edits = (values, action) => Array.from(values, ([itemId, draft]) => [itemId, {
      text: draft.text, ...(action ? { assignee: draft.assignee } : {}),
      baseRevision: draft.baseRevision, uncertain: !!draft.uncertain, conflict: !!draft.conflict,
    }]);
    return {
      version: 1, boardId: id, participant,
      drafts: Object.fromEntries(Object.entries(drafts).filter(([, text]) => text !== '')),
      actionDraft: { ...actionDraft }, editingId, editingActionId,
      cardEdits: edits(cardEdits, false), actionEdits: edits(actionEdits, true),
      outbox: unsent.map(({ path, body, options, error }) => ({ path, body, options, error })),
    };
  }

  function persistDrafts() {
    if (disposed) return;
    // Не уничтожаем данные неизвестной версии или повреждённый снимок
    // пустым состоянием страницы либо новым вводом без явного решения.
    const snapshot = draftSnapshot();
    const serialized = JSON.stringify(snapshot);
    const empty = !Object.keys(snapshot.drafts).length && !actionDraft.text && !actionDraft.assignee
      && !cardEdits.size && !actionEdits.size && !unsent.length;
    if (rejectedDraftsRaw !== null || draftStorageUnreadable || draftStorageDiverged) {
      if (serialized !== lastSavedDrafts && (lastSavedDrafts !== null || !empty)) draftWriteFailed = true;
      return;
    }
    if (serialized === lastSavedDrafts) return;
    try {
      if (empty) sessionStorage.removeItem(draftStorageKey);
      else sessionStorage.setItem(draftStorageKey, serialized);
      lastSavedDrafts = serialized;
      lastStoredRaw = empty ? null : serialized;
      draftWriteFailed = false;
      if (storageNotice === 'unavailable') {
        storageNotice = null;
        draftWarningEl.hidden = true;
      }
    } catch {
      draftWriteFailed = true;
      warnStorage('unavailable', 'Браузер не сохраняет черновики. Не закрывайте и не обновляйте эту вкладку: скопируйте важный текст.');
    }
  }

  function restoreDrafts(discardUnsaved = false) {
    let raw;
    try {
      raw = sessionStorage.getItem(draftStorageKey);
      draftStorageUnreadable = false;
    }
    catch {
      // Запись может работать при недоступном чтении. Не затираем вслепую
      // более новый снимок другого экземпляра страницы из этой вкладки.
      draftStorageUnreadable = true;
      warnStorage('unavailable', 'Браузер не сохраняет черновики. Не закрывайте и не обновляйте эту вкладку: скопируйте важный текст.');
      return;
    }
    // При quota/read-only storage память может содержать единственную
    // копию текста. Возврат из BFCache не равен согласию её удалить.
    if (!discardUnsaved && draftWriteFailed) {
      if (raw !== lastStoredRaw) {
        draftStorageDiverged = true;
        warnStorage('diverged', 'Текущие черновики не удалось сохранить, а сохранённая версия изменилась на другой странице этой вкладки. Текущий текст остаётся в полях и очереди; версия в хранилище не изменена. Скопируйте нужное перед выбором версии или обновлением страницы.');
      }
      return;
    }
    const clear = () => {
      for (const key of Object.keys(drafts)) delete drafts[key];
      actionDraft.text = '';
      actionDraft.assignee = '';
      cardEdits.clear();
      actionEdits.clear();
      unsent.length = 0;
      editingId = null;
      editingActionId = null;
      rejectedDraftsRaw = null;
      lastSavedDrafts = null;
      storageNotice = null;
      draftWarningEl.hidden = true;
      draftWriteFailed = false;
      draftStorageDiverged = false;
      lastStoredRaw = raw;
    };
    if (raw === null) { clear(); return; }
    try {
      const saved = JSON.parse(raw);
      const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
      const text = (value, max = 2000) => typeof value === 'string' && Array.from(value).length <= max;
      const identifier = value => typeof value === 'string' && /^[A-Za-z0-9-]{1,64}$/.test(value);
      const revision = value => Number.isSafeInteger(value) && value >= 0;
      const editsValid = (values, action) => Array.isArray(values)
        && values.every(pair => Array.isArray(pair) && pair.length === 2 && identifier(pair[0])
          && object(pair[1]) && text(pair[1].text) && (!action || text(pair[1].assignee, 60))
          && revision(pair[1].baseRevision) && typeof pair[1].uncertain === 'boolean' && typeof pair[1].conflict === 'boolean')
        && new Set(values.map(pair => pair[0])).size === values.length;
      const outboxValid = entry => {
        if (!object(entry) || !object(entry.body) || !object(entry.options)
          || typeof entry.options.idempotencyKey !== 'string' || !/^[A-Za-z0-9-]{8,64}$/.test(entry.options.idempotencyKey)
          || !text(entry.body.text) || !entry.body.text.trim() || !text(entry.error, 2000)) return false;
        if (entry.path === `/api/boards/${id}/cards`) return identifier(entry.body.columnId) && text(entry.body.author, 60);
        if (entry.path === `/api/boards/${id}/actions`) return text(entry.body.assignee, 60);
        return false;
      };
      if (!object(saved) || saved.version !== 1 || saved.boardId !== id || saved.participant !== participant
        || !object(saved.drafts) || !Object.entries(saved.drafts).every(([key, value]) => identifier(key) && text(value))
        || !object(saved.actionDraft) || !text(saved.actionDraft.text) || !text(saved.actionDraft.assignee, 60)
        || !editsValid(saved.cardEdits, false) || !editsValid(saved.actionEdits, true)
        || !Array.isArray(saved.outbox) || !saved.outbox.every(outboxValid)
        || !(saved.editingId === null || saved.cardEdits.some(([key]) => key === saved.editingId))
        || !(saved.editingActionId === null || saved.actionEdits.some(([key]) => key === saved.editingActionId))) {
        throw new Error('Некорректный снимок');
      }
      clear();
      Object.assign(drafts, saved.drafts);
      actionDraft.text = saved.actionDraft.text;
      actionDraft.assignee = saved.actionDraft.assignee;
      const restoreEdits = (target, values) => {
        for (const [itemId, draft] of values) target.set(itemId, {
          text: draft.text, ...(draft.assignee !== undefined ? { assignee: draft.assignee } : {}),
          baseRevision: draft.baseRevision, uncertain: draft.uncertain, pending: false,
          conflict: draft.uncertain || draft.conflict ? {
            loading: false,
            message: draft.uncertain ? 'Предыдущее сохранение не подтверждено. Проверьте актуальную версию.' : 'Проверьте актуальную версию перед сохранением.',
          } : null,
        });
      };
      restoreEdits(cardEdits, saved.cardEdits);
      restoreEdits(actionEdits, saved.actionEdits);
      editingId = saved.editingId;
      editingActionId = saved.editingActionId;
      for (const entry of saved.outbox) unsent.push({
        path: entry.path,
        body: entry.path.endsWith('/cards')
          ? { columnId: entry.body.columnId, text: entry.body.text, author: entry.body.author }
          : { text: entry.body.text, assignee: entry.body.assignee },
        options: { idempotencyKey: entry.options.idempotencyKey }, pending: false,
        error: entry.error || 'Предыдущая отправка не подтверждена. Можно безопасно повторить её.',
      });
      lastSavedDrafts = raw;
    } catch {
      rejectedDraftsRaw = raw;
      warnStorage('invalid', 'Не удалось восстановить сохранённые черновики: данные повреждены или имеют неподдерживаемый формат. Сохранённые данные не изменены; новые черновики пока не записываются.');
    }
  }

  // HTTP и SSE могут прийти в любом порядке. Версия доски не позволяет
  // старому ответу откатить уже показанные изменения. Счётчик подключений
  // может измениться без новой версии самой доски.
  function applyBoard(next) {
    if (disposed || next.id !== id) return false;
    if (board && (next.revision ?? 0) < (board.revision ?? 0)) return false;
    if (next.online !== undefined) online = next.online;
    if (board?.writing?.round !== next.writing?.round) writingNotice = '';
    board = next;
    boardStatus.hidden = true;
    root.hidden = false;
    render();
    ensureWritingJoined();
    return true;
  }

  function isCurrent(generation) {
    return !disposed && generation === pageGeneration;
  }

  function inactiveRequest() {
    return Object.assign(new Error('Страница уже сменилась'), { inactive: true });
  }

  async function boardRequest(method, path, body, options = {}) {
    const generation = pageGeneration;
    if (!isCurrent(generation)) throw inactiveRequest();
    const controller = new AbortController();
    const forwardAbort = () => controller.abort();
    options.signal?.addEventListener('abort', forwardAbort, { once: true });
    if (options.signal?.aborted) controller.abort();
    requests.add(controller);
    try {
      const result = await api(method, path, body, { ...options, signal: controller.signal });
      if (!isCurrent(generation)) throw inactiveRequest();
      return result;
    } catch (err) {
      if (!isCurrent(generation)) throw inactiveRequest();
      throw err;
    } finally {
      requests.delete(controller);
      options.signal?.removeEventListener('abort', forwardAbort);
    }
  }

  function reportError(err) {
    if (!disposed && !err.inactive) toast(err.message);
  }

  async function mutate(method, path, body, options) {
    const generation = pageGeneration;
    const next = await boardRequest(method, path, body, options);
    if (!isCurrent(generation)) throw inactiveRequest();
    applyBoard(next);
    return next;
  }

  async function refreshBoard(options) {
    const generation = pageGeneration;
    try {
      const next = await boardRequest('GET', `/api/boards/${id}`, undefined, options);
      if (!isCurrent(generation)) throw inactiveRequest();
      applyBoard(next);
      return next;
    } catch (err) {
      if (isCurrent(generation)) {
        if (err.status === 404) {
          leaveBoard();
          showUnavailable(root);
        } else if (err.status === 409) {
          leaveBoard();
          location.reload();
        } else if (err.status === 401) {
          leaveBoard();
          showUnavailable(root);
        }
      }
      throw err;
    }
  }

  function canWrite() {
    return (board.phase === 'write' && !writingLocked()) || board.phase === 'reveal';
  }

  function writingLocked() {
    return board.phase === 'write' && (writingPending || board.writing?.mine);
  }

  // Состав этапа сохраняется сервером и не зависит от SSE. Повтор входа
  // после reload/BFCache не добавляет второй голос и не снимает готовность.
  async function ensureWritingJoined() {
    const writing = board?.writing;
    if (disposed || board.phase !== 'write' || !writing || writing.joined
        || joiningRound !== null || joinFailedRound === writing.round) return;
    const generation = pageGeneration;
    const round = writing.round;
    joiningRound = round;
    writingNotice = '';
    renderWritingReady();
    try {
      await mutate('POST', `/api/boards/${id}/writing/join`, { round });
    } catch (err) {
      if (!isCurrent(generation)) return;
      joinFailedRound = round;
      writingNotice = 'Не удалось присоединиться к этапу. Повторите подключение, чтобы учитывалась ваша готовность.';
      if (err.status === 409) await refreshBoard().catch(() => {});
    } finally {
      if (isCurrent(generation)) {
        joiningRound = null;
        renderWritingReady();
        // За время запроса ведущий мог начать новый этап записи.
        ensureWritingJoined();
      }
    }
  }

  function hasWritingDrafts() {
    return Object.values(drafts).some(text => text.trim()) || cardEdits.size > 0
      || pendingCards.size > 0 || unsent.some(entry => entry.path.endsWith('/cards'));
  }

  async function setWritingReady() {
    const writing = board?.writing;
    if (writingPending || board.phase !== 'write' || !writing?.joined) return;
    const ready = !writing.mine;
    if (ready && hasWritingDrafts()) {
      writingNotice = 'Сначала добавьте написанные карточки и сохраните или закройте правки. Неотправленные записи тоже нужно отправить или удалить.';
      renderWritingReady();
      return;
    }
    const generation = pageGeneration;
    writingPending = true;
    writingNotice = '';
    render();
    try {
      await mutate('POST', `/api/boards/${id}/writing/ready`, {
        ready, round: writing.round, revision: writing.revision,
      });
    } catch (err) {
      if (!isCurrent(generation)) return;
      writingNotice = 'Не удалось подтвердить готовность. Проверяем состояние доски; при необходимости повторите нажатие.';
      // Запрос мог сохраниться до обрыва связи; сначала читаем результат.
      await refreshBoard().catch(() => {});
      reportError(err);
    } finally {
      if (isCurrent(generation)) {
        writingPending = false;
        render();
      }
    }
  }

  function renderWritingReady() {
    const writing = board?.writing;
    writingEl.hidden = disposed || board?.phase !== 'write' || !writing;
    if (writingEl.hidden) {
      writingEl.replaceChildren();
      return;
    }
    writingEl.replaceChildren(
      h('div', { class: 'writing-ready-copy' },
        h('strong', { role: 'status' }, `Готовы: ${writing.ready} из ${writing.participants}`),
        h('p', {}, writing.mine
          ? 'Вы закончили. Ждём остальных участников.'
          : 'Когда все нажмут «Я закончил», карточки откроются без ожидания таймера.'),
        writingNotice ? h('p', { class: 'writing-notice', role: 'alert' }, writingNotice) : null),
      writing.joined
        ? h('button', { type: 'button', class: writing.mine ? 'writing-resume' : 'primary',
          disabled: writingPending, onclick: setWritingReady, dataset: { keep: 'writing-ready' },
        }, writingPending ? 'Сохраняем…' : writing.mine ? 'Продолжить писать' : 'Я закончил')
        : h('button', { type: 'button', disabled: joiningRound !== null,
          onclick: () => { joinFailedRound = null; ensureWritingJoined(); },
        }, joiningRound !== null ? 'Подключаемся…' : 'Повторить подключение'),
    );
  }

  function focusField(keep) {
    root.querySelector(`[data-keep="${CSS.escape(keep)}"]`)?.focus({ preventScroll: true });
  }

  // Неудачная отправка не возвращается поверх нового черновика. Храним
  // исходные текст и подпись отдельно, пока пользователь не повторит её
  // или явно не удалит. Блок живёт вне перерисовываемых колонок.
  async function sendCreation(entry) {
    if (entry.pending) return;
    const generation = pageGeneration;
    entry.pending = true;
    persistDrafts(); // вместе с ключом операции — до первого сетевого запроса
    renderRecovery();
    try {
      await mutate('POST', entry.path, entry.body, entry.options);
      if (!isCurrent(generation)) return;
      unsent.splice(unsent.indexOf(entry), 1);
    } catch (err) {
      if (!isCurrent(generation)) return;
      entry.error = err.message;
      reportError(err);
    } finally {
      if (isCurrent(generation)) {
        entry.pending = false;
        persistDrafts();
        renderRecovery();
      }
    }
  }

  function renderRecovery() {
    if (disposed) return;
    if (!unsent.length) {
      recoveryEl.replaceChildren();
      return;
    }
    recoveryEl.replaceChildren(
      h('h2', { class: 'statusline' }, 'Неотправленные записи'),
      h('p', { class: 'statusline' }, 'Текст сохранён здесь. Можно повторить отправку.'),
      h('div', { class: 'columns' }, unsent.map(entry => h('article', { class: 'column card' },
        h('p', { class: 'text' }, entry.body.text),
        h('span', { class: 'muted' }, entry.body.assignee !== undefined
          ? `Ответственный: ${entry.body.assignee || 'не назначен'}`
          : `Автор: ${entry.body.author || 'анонимно'}`),
        h('p', { class: 'muted' }, entry.pending ? 'Отправка…' : entry.error),
        h('button', {
          type: 'button', disabled: entry.pending,
          onclick: () => sendCreation(entry),
        }, 'Повторить отправку'),
        h('button', {
          type: 'button', disabled: entry.pending,
          onclick: () => {
            if (!confirm('Удалить неотправленный черновик?')) return;
            unsent.splice(unsent.indexOf(entry), 1);
            persistDrafts();
            renderRecovery();
          },
        }, 'Удалить черновик'),
      ))),
    );
  }

  // --- действия ---

  async function setPhase(phase) {
    try { await mutate('POST', `/api/boards/${board.id}/phase`, { phase }); }
    catch (err) { reportError(err); }
  }

  async function setVoteLimit(value) {
    const n = parseInt(value, 10);
    if (!Number.isFinite(n)) return;
    try { await mutate('POST', `/api/boards/${board.id}/vote-limit`, { voteLimit: n }); }
    catch (err) { reportError(err); }
  }

  async function vote(cardId, voted) {
    const generation = pageGeneration;
    const card = currentItem('card', cardId);
    if (!card) return;
    const target = card.groupId || cardId;
    if (pendingVotes.has(target)) return;
    pendingVotes.set(target, voted);
    render();
    try { await mutate('POST', `/api/boards/${board.id}/cards/${cardId}/vote`, { voted }); }
    catch (err) { reportError(err); }
    finally {
      if (isCurrent(generation)) {
        pendingVotes.delete(target);
        render();
      }
    }
  }

  async function groupCards(cardId, targetId) {
    try { await mutate('POST', `/api/boards/${board.id}/cards/${cardId}/group`, { targetCardId: targetId }); }
    catch (err) { reportError(err); }
  }

  async function ungroupCard(cardId) {
    try { await mutate('DELETE', `/api/boards/${board.id}/cards/${cardId}/group`); }
    catch (err) { reportError(err); }
  }

  async function setActionDone(actionId, done) {
    const generation = pageGeneration;
    if (pendingDone.has(actionId)) return;
    pendingDone.set(actionId, done);
    render();
    try { await mutate('POST', `/api/boards/${board.id}/actions/${actionId}/done`, { done }); }
    catch (err) { reportError(err); }
    finally {
      if (isCurrent(generation)) {
        pendingDone.delete(actionId);
        render();
      }
    }
  }

  async function removeAction(action) {
    if (!confirm('Удалить решение?')) return;
    try { await mutate('DELETE', `/api/boards/${board.id}/actions/${action.id}`); }
    catch (err) { reportError(err); }
  }

  // minutes === 0 останавливает таймер.
  async function setTimer(minutes) {
    try { await mutate('POST', `/api/boards/${board.id}/timer`, { minutes }); }
    catch (err) { reportError(err); }
  }

  async function removeCard(card) {
    if (!confirm('Удалить карточку?')) return;
    try { await mutate('DELETE', `/api/boards/${board.id}/cards/${card.id}`); }
    catch (err) { reportError(err); }
  }

  // Следующее ретро: та же команда, тот же шаблон, незакрытые решения
  // переезжают. После создания переходим на новую доску.
  async function createNextBoard() {
    const generation = pageGeneration;
    const title = prompt('Название следующего ретро', board.title);
    if (title === null) return;
    try {
      const next = await boardRequest('POST', `/api/boards/${board.id}/next`, { title });
      if (!isCurrent(generation)) return;
      rememberBoard(next.id, next.title);
      location.href = appPath('/b/' + next.id);
    } catch (err) {
      reportError(err);
    }
  }

  async function addMember() {
    const id = window.prompt('Вставьте ID коллеги. Он может скопировать его на главной странице Retro или по ссылке на доску.');
    if (!id?.trim()) return;
    try {
      await mutate('POST', `/api/boards/${board.id}/members`, { participantId: id.trim() });
      toast('Доступ открыт. Коллега может открыть ссылку на доску.');
    } catch (err) {
      reportError(err);
    }
  }

  // --- рендер ---

  function render() {
    if (!board || disposed) return;
    if (composing) {
      renderPending = true;
      return;
    }
    renderPending = false;
    document.title = `${board.title} — Ретро`;
    titleEl.textContent = board.title;
    const saved = captureFocus();
    const scrollLeft = columnsEl.scrollLeft;
    renderPhases();
    renderChain();
    renderTools();
    renderWritingReady();
    renderTimer();
    renderColumns();
    columnsEl.scrollLeft = scrollLeft;
    restoreFocus(saved);
    persistDrafts();
  }

  // Таймер считает на клиенте от timerEndsAt, который приходит с сервера и
  // одинаков для всех. Часы сервера и браузера могут расходиться на секунды —
  // для ретро это не важно. Тикает только текст таймера, не вся доска.
  let timerInterval = null;
  let timerNotified = null; // timerEndsAt, о котором уже сообщили «время вышло»

  function renderTimer() {
    const ends = board && board.timerEndsAt;
    if (!ends) {
      clearInterval(timerInterval);
      timerInterval = null;
      timerEl.hidden = true;
      return;
    }
    const left = Math.round((new Date(ends) - Date.now()) / 1000);
    if (left <= 0) {
      clearInterval(timerInterval);
      timerInterval = null;
      timerEl.textContent = 'Время вышло';
      timerEl.className = 'timer over';
      if (timerNotified !== ends) {
        timerNotified = ends;
        // Сервер сам переключит этап и пришлёт обновление доски.
        toast(board.phase === 'discuss' ? 'Время вышло' : 'Время вышло — переходим к следующему этапу');
      }
    } else {
      const m = Math.floor(left / 60);
      const s = String(left % 60).padStart(2, '0');
      timerEl.textContent = `Осталось ${m}:${s}`;
      timerEl.className = 'timer' + (left <= 60 ? ' soon' : '');
      if (!timerInterval) timerInterval = setInterval(renderTimer, 1000);
    }
    timerEl.hidden = false;
  }

  function renderPhases() {
    const current = Math.max(0, PHASES.findIndex(p => p.id === board.phase));
    phaseNav.replaceChildren(h('ol', {}, PHASES.map((p, i) => {
      const state = i < current ? 'done' : i === current ? 'current' : 'next';
      const parts = [h('span', { class: 'phase-num' }, String(i + 1)), h('span', { class: 'phase-label' }, p.label)];
      const inner = board.isOwner
        ? h('button', {
            type: 'button', class: 'phase', disabled: i === current,
            'aria-current': i === current ? 'step' : null,
            title: i === current ? 'Текущая фаза' : `Перейти: ${p.label}`,
            onclick: () => setPhase(p.id),
          }, parts)
        : h('span', { class: 'phase', 'aria-current': i === current ? 'step' : null }, parts);
      return h('li', { class: state }, inner);
    })));
    let hint = PHASES[current].hint;
    if (board.phase === 'vote') {
      hint = `Отметьте самое важное — по одному голосу на карточку, всего ${board.voteLimit}.`;
    }
    if (board.phase === 'reveal' && board.isOwner) {
      hint += ' Перетащите карточку на другую, чтобы объединить похожие.';
    }
    hintEl.textContent = hint;
  }

  // Цепочка ретро: ссылки на предыдущее и следующее и выпадающая история.
  // История грузится при открытии, а не с каждым событием доски.
  function renderChain() {
    if (!board.prevId && !board.nextId) {
      chainEl.replaceChildren();
      return;
    }
    const list = h('ol', { class: 'history' });
    const details = h('details', { class: 'history-box' },
      h('summary', {}, 'История'),
      list);
    details.addEventListener('toggle', async () => {
      const generation = pageGeneration;
      if (!details.open) return;
      list.replaceChildren(h('li', { class: 'muted' }, 'Загрузка…'));
      try {
        const chain = await boardRequest('GET', `/api/boards/${board.id}/history`);
        if (!isCurrent(generation)) return;
        list.replaceChildren(...chain.map((item, i) => h('li', { class: item.current ? 'current' : '' },
          h('span', { class: 'history-num' }, `${i + 1}.`),
          item.current
            ? h('span', { class: 'history-title' }, item.title)
            : h('a', { href: '/b/' + item.id, class: 'history-title' }, item.title),
          h('span', { class: 'history-meta' },
            new Date(item.createdAt).toLocaleDateString('ru-RU'),
            item.openActions ? ` · открытых решений: ${item.openActions}` : ''))));
      } catch (err) {
        if (!isCurrent(generation)) return;
        list.replaceChildren(h('li', { class: 'muted' }, err.message));
      }
    });
    // replaceChildren превращает null в текст «null», поэтому фильтруем.
    chainEl.replaceChildren(...[
      board.prevId ? h('a', { href: '/b/' + board.prevId, class: 'chain-link', title: 'Предыдущее ретро' }, '← предыдущее') : null,
      board.nextId ? h('a', { href: '/b/' + board.nextId, class: 'chain-link', title: 'Следующее ретро' }, 'следующее →') : null,
      details,
    ].filter(Boolean));
  }

  function renderTools() {
    const items = [];
    items.push(h('label', { class: 'tool' }, 'Имя',
      h('input', {
        type: 'text', maxlength: 60, placeholder: 'Без имени',
        value: stored(NAME_KEY, ''), dataset: { keep: 'name' },
        onchange: e => store(NAME_KEY, e.target.value.trim()),
      })));
    items.push(h('label', { class: 'tool check' },
      h('input', {
        type: 'checkbox', checked: stored(ANON_KEY, '0') === '1',
        onchange: e => store(ANON_KEY, e.target.checked ? '1' : '0'),
      }),
      'анонимно'));
    if (board.phase === 'vote') {
      items.push(h('span', { class: 'tool votes-left' },
        dots(board.votesLeft), `${board.votesLeft} из ${board.voteLimit}`));
    }
    if (board.isOwner && canWrite()) {
      items.push(h('label', { class: 'tool' }, 'Голосов на человека',
        h('input', {
          type: 'number', min: 1, max: 20, value: board.voteLimit,
          dataset: { keep: 'limit' }, onchange: e => setVoteLimit(e.target.value),
        })));
    }
    if (board.isOwner) {
      if (board.timerEndsAt) {
        items.push(h('button', { type: 'button', class: 'tool', onclick: () => setTimer(0) }, 'Остановить таймер'));
      } else {
        const minutes = h('input', {
          type: 'number', min: 1, max: 120, value: stored(TIMER_KEY, '5'),
          dataset: { keep: 'timer' }, 'aria-label': 'Минуты таймера',
          onchange: e => store(TIMER_KEY, e.target.value),
        });
        const nextPhase = PHASES[PHASES.findIndex(p => p.id === board.phase) + 1];
        items.push(h('span', {
          class: 'tool',
          title: nextPhase
            ? `По истечении доска сама перейдёт к этапу «${nextPhase.label}»`
            : 'На последнем этапе таймер только отсчитывает время',
        }, 'Таймер, мин', minutes,
          h('button', {
            type: 'button',
            onclick: () => setTimer(parseInt(minutes.value, 10) || 0),
          }, 'Запустить')));
      }
    }
    items.push(h('span', { class: 'tool online-count' }, online < 0 ? 'Подключено' : `Онлайн: ${online}`));
    if (board.phase !== 'write') {
      items.push(h('a', {
        class: 'tool button', href: `/api/boards/${board.id}/export.md`, download: `retro-${board.id}.md`,
      }, 'Скачать .md'));
    }
    if (board.isOwner) {
      items.push(h('button', { type: 'button', class: 'tool', onclick: addMember }, 'Добавить участника'));
    }
    items.push(h('button', { type: 'button', class: 'tool share-button',
      onclick: () => copyText(location.origin + appPath('/b/' + board.id), 'Ссылка скопирована') }, 'Скопировать ссылку'));
    if (board.isOwner && board.phase === 'discuss' && !board.nextId) {
      items.push(h('button', {
        type: 'button', class: 'tool', onclick: createNextBoard,
        title: 'Новая доска той же команды: незакрытые решения перейдут в неё',
      }, 'Следующее ретро'));
    }
    toolsEl.replaceChildren(...items);
  }

  function renderColumns() {
    const columns = board.columns.map(col => {
      const empty = col.cards.length === 0
        ? h('p', { class: 'empty' }, canWrite() ? 'Пока пусто — добавьте первую карточку.' : 'Пусто.')
        : null;
      return h('section', { class: 'column' },
        h('header', { class: 'column-head' }, h('h2', {}, col.title), h('span', { class: 'count' }, String(col.cards.length))),
        empty,
        renderCardList(col),
        canWrite() ? renderComposer(col) : null,
      );
    });
    const actions = renderActions();
    const orphans = renderOrphanEdits();
    columnsEl.replaceChildren(...columns, ...(actions ? [actions] : []), ...(orphans ? [orphans] : []));
  }

  // Удаление записи другим клиентом не должно убирать пользовательский
  // черновик вместе с её карточкой. Показываем все такие редакторы отдельно,
  // пока пользователь явно их не закроет.
  function renderOrphanEdits() {
    const editors = [];
    for (const [kind, draftsById] of [['card', cardEdits], ['action', actionEdits]]) {
      for (const [itemId, draft] of draftsById) {
        if (currentItem(kind, itemId)) continue;
        draft.conflict ||= { loading: false, message: 'Запись больше не существует на доске.' };
        editors.push(kind === 'card' ? renderCardEditor({ id: itemId }) : renderActionEditor({ id: itemId }));
      }
    }
    if (!editors.length) return null;
    return h('section', { class: 'column', 'aria-label': 'Правки удалённых записей' },
      h('header', { class: 'column-head' }, h('h2', {}, 'Правки удалённых записей')),
      h('ul', { class: 'cards' }, editors));
  }

  // --- решения (action items) ---

  function canSendJira(action) {
    return board.jira && board.isOwner && board.phase === 'discuss' && !board.nextId
      && editingActionId !== action.id
      && !action.done && (!action.jira || action.jira.status === 'failed');
  }

  function jiraUnresolved(action) {
    return ['pending', 'unknown'].includes(action.jira?.status);
  }

  // Сохраняем выбранные версии: пришедшая во время выбора правка требует
  // нового выбора, иначе в Jira ушёл бы ещё не просмотренный ведущим текст.
  function renderJiraTools() {
    for (const [id, revision] of jiraSelected) {
      const action = board.actions.find(a => a.id === id);
      if (!action || !canSendJira(action) || action.revision !== revision) jiraSelected.delete(id);
    }
    if (!board.isOwner || board.phase !== 'discuss' || !board.actions.length) return null;
    return h('div', { class: 'jira-tools' },
      h('strong', {}, 'Задачи в Jira'),
      h('p', { class: 'muted' }, board.nextId
        ? 'Создавайте задачи из последнего ретро команды.'
        : board.jira
          ? `Проект ${board.jira.project}. Выберите решения для отправки. Имя ответственного попадёт в описание.`
          : 'Jira пока не подключена. Попросите администратора настроить подключение на сервере Retro.'),
      board.jira && !board.nextId ? h('button', {
        type: 'button', class: 'primary small', disabled: jiraBusy || !jiraSelected.size,
        onclick: createJiraSelected,
      }, jiraBusy ? 'Создаём задачи…' : `Создать в Jira (${jiraSelected.size})`) : null,
      jiraNotice ? h('p', { class: 'jira-notice', role: 'status' }, jiraNotice) : null);
  }

  async function createJiraSelected() {
    if (jiraBusy || !jiraSelected.size) return;
    const generation = pageGeneration;
    const selected = [...jiraSelected];
    jiraBusy = true;
    jiraNotice = '';
    render();
    let created = 0;
    try {
      for (const [actionId, revision] of selected) {
        if (!isCurrent(generation)) return;
        const result = await mutate('POST', `/api/boards/${id}/actions/${actionId}/jira`, { revision });
        const action = result.actions.find(a => a.id === actionId);
        if (action?.jira?.status === 'created') { created++; jiraSelected.delete(actionId); }
        jiraNotice = `Создано задач: ${created} из ${selected.length}.`;
        render();
      }
      if (created < selected.length) jiraNotice += ' Проверьте сообщения у решений.';
    } catch (err) {
      if (!isCurrent(generation)) return;
      jiraNotice = `Создано задач: ${created}. ${err.message} Остальные решения не отправлялись.`;
      // При потере HTTP-ответа сохранённое состояние важнее локального флага.
      try { await refreshBoard(); } catch { /* проверка/повтор доступны вручную */ }
    } finally {
      if (isCurrent(generation)) { jiraBusy = false; render(); }
    }
  }

  async function checkJira(action) {
    if (jiraChecking.has(action.id)) return;
    const generation = pageGeneration;
    jiraChecking.add(action.id);
    render();
    try {
      await mutate('POST', `/api/boards/${id}/actions/${action.id}/jira/check`, {});
      toast('Задача найдена, ссылка сохранена');
    } catch (err) { reportError(err); }
    finally { if (isCurrent(generation)) { jiraChecking.delete(action.id); render(); } }
  }

  function jiraLink(url, text) {
    // Не доверяем ссылкам из импортированных/старых данных.
    try {
      const parsed = new URL(url);
      if (!['https:', 'http:'].includes(parsed.protocol) || parsed.username || parsed.password) return null;
      return h('a', { href: parsed.href, target: '_blank', rel: 'noopener noreferrer' }, text);
    } catch { return null; }
  }

  function renderActionJira(action) {
    const link = action.jira;
    return h('div', { class: 'action-jira' },
      canSendJira(action) ? h('label', { class: 'jira-select' },
        h('input', { type: 'checkbox', checked: jiraSelected.has(action.id), disabled: jiraBusy,
          'aria-label': `Выбрать для Jira: ${action.text}`, dataset: { keep: `jira-select:${action.id}` },
          onchange: e => {
            if (e.target.checked) jiraSelected.set(action.id, action.revision);
            else jiraSelected.delete(action.id);
            render();
          },
        }), 'В Jira') : null,
      link?.status === 'created' ? h('span', { class: 'jira-created' },
        jiraLink(link.url, link.key), h('span', { class: 'muted' }, ' · изменения в Retro не меняют задачу')) : null,
      link?.status === 'failed' ? h('p', { class: 'jira-error' }, link.error) : null,
      jiraUnresolved(action) ? h('div', { class: 'jira-recovery' },
        h('p', {}, link.error || 'Отправка в Jira ещё не подтверждена. Если ожидание затянулось, проверьте результат.'),
        board.isOwner && board.jira ? h('button', { type: 'button', class: 'small',
          disabled: jiraBusy || jiraChecking.has(action.id), onclick: () => checkJira(action),
        }, jiraChecking.has(action.id) ? 'Проверяем…' : 'Проверить в Jira') : null,
        jiraLink(link.searchUrl, 'Открыть поиск в Jira')) : null);
  }

  // Панель решений появляется после фазы записи: пока карточки скрыты,
  // договариваться не о чем. Если решения уже есть, панель видна всегда.
  function renderActions() {
    const open = board.phase !== 'write';
    if (!open && board.actions.length === 0) return null;
    const done = board.actions.filter(a => pendingDone.get(a.id) ?? a.done).length;
    const empty = board.actions.length === 0
      ? h('p', { class: 'empty' }, 'Что команда решила сделать и кто отвечает.')
      : null;
    return h('section', { class: 'column actions', 'aria-label': 'Решения' },
      h('header', { class: 'column-head' },
        h('h2', {}, 'Решения'),
        h('span', { class: 'count' }, board.actions.length ? `${done} из ${board.actions.length}` : '0')),
      renderJiraTools(),
      empty,
      h('ul', { class: 'cards' }, board.actions.map(renderAction)),
      open ? renderActionComposer() : null,
    );
  }

  function renderAction(action) {
    if (editingActionId === action.id) return renderActionEditor(action);
    const editable = (action.mine || board.isOwner) && !jiraUnresolved(action);
    const done = pendingDone.get(action.id) ?? action.done;
    const checkbox = h('input', {
      type: 'checkbox', checked: done, disabled: !editable || pendingDone.has(action.id),
      'aria-label': done ? 'Сделано' : 'Не сделано',
      onchange: e => setActionDone(action.id, e.target.checked),
    });
    const controls = [];
    if (editable) {
      controls.push(h('button', { type: 'button', class: 'link', onclick: () => {
        editingActionId = action.id;
        render();
        focusField('action-edit:' + action.id);
      } }, 'Изменить'));
      controls.push(h('button', { type: 'button', class: 'link danger', onclick: () => removeAction(action) }, 'Удалить'));
    }
    return h('li', { class: 'card action' + (done ? ' done' : '') },
      h('div', { class: 'action-row' },
        checkbox,
        h('p', { class: 'text' }, action.text)),
      h('div', { class: 'card-foot' },
        h('span', { class: 'meta' },
          h('span', { class: 'assignee' }, action.assignee || 'ответственный не назначен'),
          action.carried ? h('span', { class: 'carried', title: 'Перенесено из предыдущего ретро' }, 'перенесено') : null),
        h('span', { class: 'card-actions' }, controls)),
      renderActionJira(action));
  }

  function renderActionComposer() {
    const text = h('textarea', {
      rows: 2, maxlength: 2000, placeholder: 'Что сделать',
      value: actionDraft.text, dataset: { keep: 'action-text' },
      oninput: e => { actionDraft.text = e.target.value; persistDrafts(); },
      onkeydown: e => {
        if (e.isComposing) return;
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
      },
    });
    const assignee = h('input', {
      type: 'text', maxlength: 60, placeholder: 'Ответственный',
      value: actionDraft.assignee, dataset: { keep: 'action-assignee' },
      oninput: e => { actionDraft.assignee = e.target.value; persistDrafts(); },
      onkeydown: e => { if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); submit(); } },
    });
    const btn = h('button', {
      type: 'button', class: 'primary small', disabled: actionPending,
      dataset: { send: 'action' }, onclick: submit,
    }, 'Записать');

    async function submit() {
      const generation = pageGeneration;
      if (actionPending) return;
      const value = text.value.trim();
      if (!value) return;
      const who = assignee.value.trim();
      actionDraft.text = '';
      text.value = '';
      actionPending = true;
      btn.disabled = true;
      text.focus();
      const path = `/api/boards/${board.id}/actions`;
      const body = { text: value, assignee: who };
      const options = { idempotencyKey: randomId() };
      const entry = { path, body, options, error: '', pending: false };
      unsent.push(entry);
      try {
        await sendCreation(entry);
      } finally {
        if (isCurrent(generation)) {
          actionPending = false;
          const live = columnsEl.querySelector('[data-send="action"]');
          if (live) live.disabled = false;
        }
      }
    }
    return h('div', { class: 'composer' }, text, h('div', { class: 'composer-row' }, assignee, btn));
  }

  function currentItem(kind, itemId) {
    return kind === 'action'
      ? board.actions.find(a => a.id === itemId)
      : board.columns.flatMap(col => col.cards).find(card => card.id === itemId);
  }

  async function loadConflict(draft, kind, itemId, expectedRevision, message) {
    const generation = pageGeneration;
    draft.conflict = { loading: true, expectedRevision, message };
    render();
    try {
      await refreshBoard();
      if (!isCurrent(generation)) return;
      draft.conflict.loading = false;
    } catch (err) {
      if (!isCurrent(generation)) return;
      draft.conflict.loading = false;
      draft.conflict.error = err.message;
    }
    render();
  }

  function renderConflict(draft, kind, itemId, save) {
    const conflict = draft.conflict;
    if (!conflict) return null;
    const latest = currentItem(kind, itemId);
    const waiting = draft.pending || conflict.loading;
    return h('div', { class: 'edit-conflict', role: 'status' },
      h('strong', {}, draft.uncertain ? 'Не удалось подтвердить сохранение' : 'Не удалось сохранить изменения'),
      h('p', {}, conflict.message),
      h('p', {}, 'Ваши правки остались в редакторе. Выберите, какую версию сохранить.'),
      conflict.loading ? h('p', {}, 'Загружаем актуальную версию…')
        : conflict.error ? h('p', {}, conflict.error)
        : latest ? h('div', {},
          h('strong', {}, 'Сейчас на доске'),
          h('p', { class: 'text' }, latest.text),
          kind === 'action' ? h('p', {}, `Ответственный: ${latest.assignee || 'не назначен'}`) : null)
        : h('p', {}, 'Запись уже удалена. Скопируйте нужный текст из редактора.'),
      conflict.error ? h('button', {
        type: 'button', disabled: waiting,
        onclick: () => loadConflict(draft, kind, itemId, conflict.expectedRevision, conflict.message),
      }, 'Обновить актуальную версию') : null,
      latest && !conflict.error && !conflict.loading ? h('div', { class: 'conflict-actions' },
        h('button', {
          type: 'button', disabled: waiting,
          onclick: () => {
            draft.text = latest.text;
            if (kind === 'action') draft.assignee = latest.assignee;
            draft.baseRevision = latest.revision ?? 0;
            draft.uncertain = false;
            draft.conflict = null;
            render();
            focusField((kind === 'action' ? 'action-edit:' : 'edit:') + itemId);
          },
        }, 'Загрузить актуальную версию'),
        h('button', {
          type: 'button', disabled: waiting,
          onclick: () => save(latest.revision ?? 0),
        }, 'Сохранить мой вариант поверх')) : null,
    );
  }

  function renderActionEditor(action) {
    if (!actionEdits.has(action.id)) {
      actionEdits.set(action.id, {
        text: action.text, assignee: action.assignee,
        baseRevision: action.revision ?? 0, pending: false, conflict: null,
      });
    }
    const draft = actionEdits.get(action.id);
    const text = h('textarea', {
      rows: 3, maxlength: 2000, value: draft.text, disabled: draft.pending,
      dataset: { keep: 'action-edit:' + action.id },
      oninput: e => { draft.text = e.target.value; persistDrafts(); },
      onkeydown: e => {
        if (e.isComposing) return;
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); save(); }
        if (e.key === 'Escape') { cancel(); }
      },
    });
    const assignee = h('input', {
      type: 'text', maxlength: 60, placeholder: 'Ответственный', value: draft.assignee, disabled: draft.pending,
      dataset: { keep: 'action-edit-assignee:' + action.id },
      oninput: e => { draft.assignee = e.target.value; persistDrafts(); },
      onkeydown: e => {
        if (e.isComposing) return;
        if (e.key === 'Enter') { e.preventDefault(); save(); }
        if (e.key === 'Escape') { cancel(); }
      },
    });
    const cancel = () => {
      if (draft.pending) return;
      actionEdits.delete(action.id);
      if (editingActionId === action.id) editingActionId = null;
      render();
    };
    const save = async (expectedRevision) => {
      const generation = pageGeneration;
      const value = draft.text.trim();
      if (!value || draft.pending || (draft.conflict && expectedRevision === undefined)) return;
      const revision = expectedRevision ?? draft.baseRevision;
      draft.pending = true;
      draft.uncertain = true;
      render();
      try {
        await mutate('PATCH', `/api/boards/${board.id}/actions/${action.id}`, {
          text: value, assignee: draft.assignee.trim(), revision,
        });
        if (!isCurrent(generation)) return;
        draft.uncertain = false;
        // Более новый SSE мог сообщить об удалении уже после применения
        // PATCH на сервере. Сохраняем доступ к тексту и в этом случае.
        if (currentItem('action', action.id)) {
          actionEdits.delete(action.id);
          if (editingActionId === action.id) editingActionId = null;
        }
      } catch (err) {
        if (!isCurrent(generation)) return;
        draft.uncertain = !!err.unknownOutcome;
        draft.pending = false;
        if (err.status === 409 || draft.uncertain) await loadConflict(draft, 'action', action.id, revision, err.message);
        else toast(err.message);
      } finally {
        if (isCurrent(generation)) {
          draft.pending = false;
          render();
        }
      }
    };
    const el = h('li', { class: 'card editing' }, text, assignee,
      renderConflict(draft, 'action', action.id, save),
      h('div', { class: 'card-actions' },
        h('button', { type: 'button', class: 'primary small', disabled: draft.pending || !!draft.conflict, onclick: () => save() }, 'Сохранить'),
        h('button', { type: 'button', class: 'small', disabled: draft.pending, onclick: cancel },
          currentItem('action', action.id) ? 'Отмена' : 'Закрыть черновик')));
    return el;
  }

  function renderComposer(col) {
    const ta = h('textarea', {
      rows: 2, maxlength: 2000, placeholder: 'Новая карточка',
      value: drafts[col.id] || '', dataset: { keep: 'draft:' + col.id },
      oninput: e => { drafts[col.id] = e.target.value; persistDrafts(); },
      onkeydown: e => {
        if (e.isComposing) return;
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
      },
    });
    const btn = h('button', {
      type: 'button', class: 'primary small', disabled: pendingCards.has(col.id),
      dataset: { send: 'card:' + col.id }, onclick: submit,
    }, 'Добавить');

    async function submit() {
      const generation = pageGeneration;
      if (pendingCards.has(col.id)) return;
      const text = ta.value.trim();
      if (!text) return;
      // Очищаем до запроса: событие SSE может перерисовать колонку раньше,
      // чем вернётся ответ, и новая textarea возьмёт текст из drafts.
      drafts[col.id] = '';
      ta.value = '';
      pendingCards.add(col.id);
      btn.disabled = true;
      ta.focus();
      const path = `/api/boards/${board.id}/cards`;
      const body = { columnId: col.id, text, author: authorName() };
      const options = { idempotencyKey: randomId() };
      const entry = { path, body, options, error: '', pending: false };
      unsent.push(entry);
      try {
        await sendCreation(entry);
      } finally {
        if (isCurrent(generation)) {
          pendingCards.delete(col.id);
          const live = columnsEl.querySelector(`[data-send="card:${CSS.escape(col.id)}"]`);
          if (live) live.disabled = false;
        }
      }
    }
    return h('div', { class: 'composer' }, ta, btn);
  }

  // --- группы и перетаскивание ---

  // Группирует только ведущий, и только когда карточки видны всем.
  function canGroup() {
    return board.isOwner && board.phase !== 'write';
  }

  // Карточки одной группы приходят с сервера подряд, поэтому группу можно
  // собрать за один проход по списку.
  function renderCardList(col) {
    const items = [];
    for (let i = 0; i < col.cards.length;) {
      const card = col.cards[i];
      if (!card.groupId) {
        items.push(renderCard(card, false));
        i++;
        continue;
      }
      const members = [];
      while (i < col.cards.length && col.cards[i].groupId === card.groupId) members.push(col.cards[i++]);
      items.push(renderGroup(members));
    }
    return h('ul', { class: 'cards' }, items);
  }

  function voteControl(voted, count, cardId) {
    if (board.phase === 'vote') {
      const target = currentItem('card', cardId)?.groupId || cardId;
      const selected = pendingVotes.get(target) ?? voted;
      return h('button', {
        type: 'button', class: 'vote' + (selected ? ' voted' : ''), disabled: pendingVotes.has(target),
        'aria-pressed': selected ? 'true' : 'false',
        title: selected ? 'Снять голос' : 'Отдать голос',
        onclick: () => vote(cardId, !voted),
      }, h('i', { class: 'dot' }), String(count));
    }
    if (board.phase === 'discuss') {
      return h('span', { class: 'vote static' }, h('i', { class: 'dot' }), String(count));
    }
    return null;
  }

  function renderGroup(members) {
    const head = members[0];
    const el = h('li', { class: 'group', 'aria-label': `Группа из ${members.length} карточек` },
      h('ul', { class: 'cards' }, members.map(c => renderCard(c, true))),
      h('div', { class: 'group-foot' },
        h('span', { class: 'meta' }, `Группа · ${members.length}`),
        h('span', { class: 'card-actions' }, voteControl(head.groupVoted, head.groupVotes, head.id))));
    attachDrop(el, head.id);
    return el;
  }

  let dragging = null; // id перетаскиваемой карточки

  function attachDrag(el, cardId) {
    if (!canGroup()) return;
    el.draggable = true;
    el.addEventListener('dragstart', e => {
      dragging = cardId;
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', cardId);
      el.classList.add('dragging');
    });
    el.addEventListener('dragend', () => {
      dragging = null;
      el.classList.remove('dragging');
    });
  }

  function attachDrop(el, targetId) {
    if (!canGroup()) return;
    el.addEventListener('dragover', e => {
      if (!dragging || dragging === targetId) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      el.classList.add('drop-target');
    });
    el.addEventListener('dragleave', () => el.classList.remove('drop-target'));
    el.addEventListener('drop', e => {
      e.preventDefault();
      e.stopPropagation(); // карточка внутри группы: не отдавать событие группе
      el.classList.remove('drop-target');
      const id = dragging || e.dataTransfer.getData('text/plain');
      if (id && id !== targetId) groupCards(id, targetId);
    });
  }

  function renderCard(card, inGroup) {
    if (card.hidden) {
      return h('li', { class: 'card hidden', 'aria-label': 'Скрытая карточка другого участника' });
    }
    if (editingId === card.id) return renderCardEditor(card);

    const editable = canWrite() && (card.mine || board.isOwner);
    const deletable = !writingLocked() && ((card.mine && canWrite()) || board.isOwner);

    const meta = [h('span', { class: 'author' }, card.author || 'анонимно')];
    if (card.mine) meta.push(h('span', { class: 'mine-badge' }, 'ваша'));

    const actions = [];
    if (!inGroup) actions.push(voteControl(card.voted, card.votes, card.id));
    if (inGroup && canGroup()) {
      actions.push(h('button', { type: 'button', class: 'link', onclick: () => ungroupCard(card.id) }, 'Из группы'));
    }
    if (editable) {
      actions.push(h('button', { type: 'button', class: 'link', onclick: () => {
        editingId = card.id;
        render();
        focusField('edit:' + card.id);
      } }, 'Изменить'));
    }
    if (deletable) {
      actions.push(h('button', { type: 'button', class: 'link danger', onclick: () => removeCard(card) }, 'Удалить'));
    }

    const el = h('li', { class: 'card' + (card.mine ? ' mine' : '') },
      h('p', { class: 'text' }, card.text),
      h('div', { class: 'card-foot' },
        h('span', { class: 'meta' }, meta),
        h('span', { class: 'card-actions' }, actions)));
    attachDrag(el, card.id);
    attachDrop(el, card.id);
    return el;
  }

  function renderCardEditor(card) {
    if (!cardEdits.has(card.id)) cardEdits.set(card.id, {
      text: card.text, baseRevision: card.revision ?? 0, pending: false, conflict: null,
    });
    const draft = cardEdits.get(card.id);
    const ta = h('textarea', {
      rows: 3, maxlength: 2000, value: draft.text, disabled: draft.pending,
      dataset: { keep: 'edit:' + card.id },
      oninput: e => { draft.text = e.target.value; persistDrafts(); },
      onkeydown: e => {
        if (e.isComposing) return;
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); save(); }
        if (e.key === 'Escape') { cancel(); }
      },
    });
    const cancel = () => {
      if (draft.pending) return;
      cardEdits.delete(card.id);
      if (editingId === card.id) editingId = null;
      render();
    };
    const save = async (expectedRevision) => {
      const generation = pageGeneration;
      const text = draft.text.trim();
      if (!text || draft.pending || (draft.conflict && expectedRevision === undefined)) return;
      const revision = expectedRevision ?? draft.baseRevision;
      draft.pending = true;
      draft.uncertain = true;
      render();
      try {
        await mutate('PATCH', `/api/boards/${board.id}/cards/${card.id}`, { text, revision });
        if (!isCurrent(generation)) return;
        draft.uncertain = false;
        if (currentItem('card', card.id)) {
          cardEdits.delete(card.id);
          if (editingId === card.id) editingId = null;
        }
      } catch (err) {
        if (!isCurrent(generation)) return;
        draft.uncertain = !!err.unknownOutcome;
        draft.pending = false;
        if (err.status === 409 || draft.uncertain) await loadConflict(draft, 'card', card.id, revision, err.message);
        else toast(err.message);
      } finally {
        if (isCurrent(generation)) {
          draft.pending = false;
          render();
        }
      }
    };
    const el = h('li', { class: 'card editing' }, ta,
      renderConflict(draft, 'card', card.id, save),
      h('div', { class: 'card-actions' },
        h('button', { type: 'button', class: 'primary small', disabled: draft.pending || !!draft.conflict, onclick: () => save() }, 'Сохранить'),
        h('button', { type: 'button', class: 'small', disabled: draft.pending, onclick: cancel },
          currentItem('card', card.id) ? 'Отмена' : 'Закрыть черновик')));
    return el;
  }

  // --- подключение ---

  function stopPolling() {
    polling = false;
    clearTimeout(pollTimer);
    pollTimer = null;
    pollController?.abort();
  }

  async function pollBoard() {
    if (!polling || disposed || pollInFlight) return;
    const generation = pageGeneration;
    pollInFlight = true;
    const controller = new AbortController();
    pollController = controller;
    const timeout = setTimeout(() => controller.abort(), 10000);
    try {
      await refreshBoard({ signal: controller.signal });
    } catch {
      // Статус потери связи уже показан; следующий запрос повторится позже.
    } finally {
      clearTimeout(timeout);
      if (isCurrent(generation)) {
        pollInFlight = false;
        pollController = null;
        if (polling) pollTimer = setTimeout(pollBoard, 3000);
      }
    }
  }

  function startPolling() {
    if (polling || disposed) return;
    polling = true;
    setStatus('Нет связи с обновлениями, проверяем доску каждые 3 секунды…');
    if (root.hidden) boardStatus.textContent = 'Восстанавливаем связь с доской…';
    pollBoard();
  }

  function connect() {
    eventSource?.close();
    const stream = new EventSource(appPath(`/api/boards/${id}/events`));
    eventSource = stream;
    stream.addEventListener('board', e => {
      if (disposed || eventSource !== stream) return;
      const data = JSON.parse(e.data);
      if (data.participant !== participant) {
        stream.close();
        root.hidden = true;
        boardStatus.hidden = false;
        startPolling();
        return;
      }
      stopPolling();
      setStatus('');
      applyBoard(data);
    });
    // Открытый поток может ещё буферизоваться прокси: останавливаем GET
    // только после реального события board, а не при onopen.
    stream.onerror = () => { if (eventSource === stream) startPolling(); };
  }

  function leaveBoard() {
    if (disposed) return;
    persistDrafts();
    disposed = true;
    root.hidden = true;
    boardStatus.hidden = true;
    document.title = 'Ретро';
    pageGeneration++;
    for (const controller of requests) controller.abort();
    stopPolling();
    clearInterval(timerInterval);
    timerInterval = null;
    cancelAnimationFrame(renderFrame);
    eventSource?.close();
    eventSource = null;
  }
  window.addEventListener('pagehide', leaveBoard);
  window.addEventListener('pageshow', e => {
    if (!e.persisted) return;
    if (!columnsEl.isConnected) {
      location.reload();
      return;
    }
    boardStatus.hidden = false;
    boardStatus.textContent = 'Проверяем доступ к доске…';
    // Другой экземпляр этой доски в истории вкладки мог изменить или
    // удалить сохранённые черновики. Его снимок теперь актуальнее памяти
    // страницы из BFCache. Старые запросы уже отменены и сменили поколение.
    pendingCards.clear();
    pendingDone.clear();
    pendingVotes.clear();
    jiraBusy = false;
    writingPending = false;
    joiningRound = null;
    joinFailedRound = null;
    writingNotice = '';
    jiraChecking.clear();
    actionPending = false;
    pollInFlight = false;
    pollController = null;
    composing = false;
    renderPending = false;
    for (const entry of unsent) {
      entry.pending = false;
      entry.error ||= 'Предыдущая отправка не подтверждена. Можно безопасно повторить её.';
    }
    for (const draft of [...cardEdits.values(), ...actionEdits.values()]) {
      draft.pending = false;
      if (draft.uncertain) draft.conflict = {
        loading: false, message: 'Предыдущее сохранение не подтверждено. Проверьте актуальную версию.',
      };
    }
    restoreDrafts();
    disposed = false;
    renderRecovery();
    // До свежего HTTP/SSE снимка нельзя предлагать повтор PATCH по старой
    // версии. Удаляем и прежнее активное поле: captureFocus не должен
    // вернуть его устаревший текст поверх восстановленного черновика.
    columnsEl.replaceChildren(h('p', { class: 'muted' }, 'Обновляем доску…'));
    toolsEl.replaceChildren();
    writingEl.replaceChildren();
    writingEl.hidden = true;
    phaseNav.replaceChildren();
    chainEl.replaceChildren();
    connect();
    refreshBoard().catch(err => { if (!err.inactive) startPolling(); });
  });

  (async () => {
    const generation = pageGeneration;
    try {
      await refreshBoard();
      if (!isCurrent(generation)) return;
      rememberBoard(board.id, board.title);
      connect();
    } catch (err) {
      if (!isCurrent(generation)) return;
      boardStatus.hidden = true;
      root.hidden = false;
      root.replaceChildren(h('div', { class: 'notfound' },
        h('h1', {}, 'Не удалось открыть доску'),
        h('p', {}, err.message),
        h('a', { href: '/', class: 'button' }, 'На главную')));
    }
  })();
}

// ---------- Маршрутизация ----------

document.addEventListener('DOMContentLoaded', () => {
  const root = document.getElementById('app');
  if (!participant) {
    root.textContent = 'Не удалось открыть сессию Retro. Обновите страницу.';
    return;
  }
  const m = location.pathname.slice(basePath.length).match(/^\/b\/([A-Za-z0-9]+)\/?$/);
  if (document.querySelector('meta[name="retro-board-unavailable"]')) showUnavailable(root);
  else if (m) renderBoardPage(root, m[1]);
  else renderHome(root);
});
