'use strict';

// ---------- Хранилище браузера ----------

const PID_KEY = 'retro.participant';
const NAME_KEY = 'retro.name';
const ANON_KEY = 'retro.anonymous';
const RECENT_KEY = 'retro.recent';
const TIMER_KEY = 'retro.timer';

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

const participant = (() => {
  let id = stored(PID_KEY, '');
  if (!/^[a-f0-9]{32}$/.test(id)) {
    id = randomId();
    store(PID_KEY, id);
  }
  return id;
})();

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
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

async function api(method, path, body) {
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: { 'Content-Type': 'application/json', 'X-Participant': participant },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new Error('Сервер недоступен. Проверьте соединение.');
  }
  if (!res.ok) {
    let message = `Ошибка ${res.status}`;
    try {
      const data = await res.json();
      if (data.error) message = data.error;
    } catch { /* тело не JSON */ }
    const err = new Error(message);
    err.status = res.status;
    throw err;
  }
  return res.json();
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
  const submit = h('button', { type: 'submit', class: 'primary' }, 'Создать доску');

  const form = h('form', {
    class: 'create',
    onsubmit: async (e) => {
      e.preventDefault();
      store(NAME_KEY, nameInput.value.trim());
      submit.disabled = true;
      try {
        const board = await api('POST', '/api/boards', { title: titleInput.value, template: select.value });
        rememberBoard(board.id, board.title);
        location.href = '/b/' + board.id;
      } catch (err) {
        toast(err.message);
        submit.disabled = false;
      }
    },
  },
    h('h1', {}, 'Ретро спринта'),
    h('p', { class: 'lead' }, 'Создайте доску и отправьте команде ссылку. Регистрация не нужна.'),
    h('label', {}, 'Название', titleInput),
    h('label', {}, 'Шаблон', select),
    h('label', {}, 'Ваше имя', nameInput),
    submit,
  );
  root.replaceChildren(form);

  const recent = recentBoards();
  if (recent.length) {
    root.append(h('section', { class: 'recent' },
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
  let board = null;
  let online = 0;
  let editingId = null;
  const drafts = {};

  const titleEl = h('h1', { class: 'board-title' });
  const phaseNav = h('nav', { class: 'phases', 'aria-label': 'Фазы ретро' });
  const toolsEl = h('div', { class: 'tools' });
  const hintEl = h('p', { class: 'hint' });
  const timerEl = h('span', { class: 'timer', role: 'timer', hidden: true });
  const statusEl = h('span', { class: 'status', hidden: true });
  const columnsEl = h('main', { class: 'columns' });

  root.replaceChildren(
    h('header', { class: 'topbar' },
      h('div', { class: 'topbar-left' }, h('a', { href: '/', class: 'home' }, 'Ретро'), titleEl),
      phaseNav,
      toolsEl,
    ),
    h('div', { class: 'statusline' }, hintEl, timerEl, statusEl),
    columnsEl,
  );

  function setStatus(text) {
    statusEl.textContent = text;
    statusEl.hidden = !text;
  }

  function canWrite() {
    return board.phase === 'write' || board.phase === 'reveal';
  }

  function liveDraft(colId) {
    return columnsEl.querySelector(`[data-keep="draft:${colId}"]`);
  }

  // --- действия ---

  async function setPhase(phase) {
    try { await api('POST', `/api/boards/${board.id}/phase`, { phase }); }
    catch (err) { toast(err.message); }
  }

  async function setVoteLimit(value) {
    const n = parseInt(value, 10);
    if (!Number.isFinite(n)) return;
    try { await api('POST', `/api/boards/${board.id}/vote-limit`, { voteLimit: n }); }
    catch (err) { toast(err.message); }
  }

  async function vote(cardId) {
    try { await api('POST', `/api/boards/${board.id}/cards/${cardId}/vote`); }
    catch (err) { toast(err.message); }
  }

  // minutes === 0 останавливает таймер.
  async function setTimer(minutes) {
    try { await api('POST', `/api/boards/${board.id}/timer`, { minutes }); }
    catch (err) { toast(err.message); }
  }

  async function removeCard(card) {
    if (!confirm('Удалить карточку?')) return;
    try { await api('DELETE', `/api/boards/${board.id}/cards/${card.id}`); }
    catch (err) { toast(err.message); }
  }

  async function copyLink() {
    const url = location.origin + '/b/' + board.id;
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(url);
      } else {
        const ta = h('textarea', { value: url, readonly: true, style: 'position:fixed;left:-9999px' });
        document.body.append(ta);
        ta.select();
        document.execCommand('copy');
        ta.remove();
      }
      toast('Ссылка скопирована');
    } catch {
      toast('Не удалось скопировать. Ссылка: ' + url);
    }
  }

  // --- рендер ---

  function render() {
    if (!board) return;
    document.title = `${board.title} — Ретро`;
    titleEl.textContent = board.title;
    const saved = captureFocus();
    const scrollLeft = columnsEl.scrollLeft;
    renderPhases();
    renderTools();
    renderTimer();
    renderColumns();
    columnsEl.scrollLeft = scrollLeft;
    restoreFocus(saved);
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
        toast('Время фазы вышло');
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
    hintEl.textContent = hint;
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
        items.push(h('span', { class: 'tool' }, 'Таймер, мин', minutes,
          h('button', {
            type: 'button',
            onclick: () => setTimer(parseInt(minutes.value, 10) || 0),
          }, 'Запустить')));
      }
    }
    items.push(h('span', { class: 'tool' }, `Онлайн: ${online}`));
    if (board.phase !== 'write') {
      items.push(h('a', {
        class: 'tool button', href: `/api/boards/${board.id}/export.md`, download: `retro-${board.id}.md`,
      }, 'Скачать .md'));
    }
    items.push(h('button', { type: 'button', class: 'tool', onclick: copyLink }, 'Скопировать ссылку'));
    toolsEl.replaceChildren(...items);
  }

  function renderColumns() {
    columnsEl.replaceChildren(...board.columns.map(col => {
      const empty = col.cards.length === 0
        ? h('p', { class: 'empty' }, canWrite() ? 'Пока пусто — добавьте первую карточку.' : 'Пусто.')
        : null;
      return h('section', { class: 'column' },
        h('header', { class: 'column-head' }, h('h2', {}, col.title), h('span', { class: 'count' }, String(col.cards.length))),
        empty,
        h('ul', { class: 'cards' }, col.cards.map(renderCard)),
        canWrite() ? renderComposer(col) : null,
      );
    }));
  }

  function renderComposer(col) {
    const ta = h('textarea', {
      rows: 2, maxlength: 2000, placeholder: 'Новая карточка',
      value: drafts[col.id] || '', dataset: { keep: 'draft:' + col.id },
      oninput: e => { drafts[col.id] = e.target.value; },
      onkeydown: e => {
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
      },
    });
    const btn = h('button', { type: 'button', class: 'primary small', onclick: submit }, 'Добавить');

    async function submit() {
      const text = ta.value.trim();
      if (!text) return;
      // Очищаем до запроса: событие SSE может перерисовать колонку раньше,
      // чем вернётся ответ, и новая textarea возьмёт текст из drafts.
      drafts[col.id] = '';
      ta.value = '';
      btn.disabled = true;
      try {
        await api('POST', `/api/boards/${board.id}/cards`, { columnId: col.id, text, author: authorName() });
      } catch (err) {
        drafts[col.id] = text;
        const live = liveDraft(col.id);
        if (live) live.value = text;
        toast(err.message);
      } finally {
        btn.disabled = false;
        const live = liveDraft(col.id);
        if (live) live.focus();
      }
    }
    return h('div', { class: 'composer' }, ta, btn);
  }

  function renderCard(card) {
    if (card.hidden) {
      return h('li', { class: 'card hidden', 'aria-label': 'Скрытая карточка другого участника' });
    }
    if (editingId === card.id) return renderCardEditor(card);

    const editable = canWrite() && (card.mine || board.isOwner);
    const deletable = (card.mine && canWrite()) || board.isOwner;
    const showVotes = board.phase === 'vote' || board.phase === 'discuss';

    const meta = [h('span', { class: 'author' }, card.author || 'анонимно')];
    if (card.mine) meta.push(h('span', { class: 'mine-badge' }, 'ваша'));

    const actions = [];
    if (showVotes && board.phase === 'vote') {
      actions.push(h('button', {
        type: 'button', class: 'vote' + (card.voted ? ' voted' : ''),
        'aria-pressed': card.voted ? 'true' : 'false',
        title: card.voted ? 'Снять голос' : 'Отдать голос',
        onclick: () => vote(card.id),
      }, h('i', { class: 'dot' }), String(card.votes)));
    } else if (showVotes) {
      actions.push(h('span', { class: 'vote static' }, h('i', { class: 'dot' }), String(card.votes)));
    }
    if (editable) {
      actions.push(h('button', { type: 'button', class: 'link', onclick: () => { editingId = card.id; render(); } }, 'Изменить'));
    }
    if (deletable) {
      actions.push(h('button', { type: 'button', class: 'link danger', onclick: () => removeCard(card) }, 'Удалить'));
    }

    return h('li', { class: 'card' + (card.mine ? ' mine' : '') },
      h('p', { class: 'text' }, card.text),
      h('div', { class: 'card-foot' },
        h('span', { class: 'meta' }, meta),
        h('span', { class: 'card-actions' }, actions)));
  }

  function renderCardEditor(card) {
    const ta = h('textarea', {
      rows: 3, maxlength: 2000, value: card.text, dataset: { keep: 'edit:' + card.id },
      onkeydown: e => {
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); save(); }
        if (e.key === 'Escape') { cancel(); }
      },
    });
    const cancel = () => { editingId = null; render(); };
    const save = async () => {
      const text = ta.value.trim();
      if (!text) return;
      try {
        await api('PATCH', `/api/boards/${board.id}/cards/${card.id}`, { text });
        editingId = null;
        render();
      } catch (err) {
        toast(err.message);
      }
    };
    const el = h('li', { class: 'card editing' }, ta,
      h('div', { class: 'card-actions' },
        h('button', { type: 'button', class: 'primary small', onclick: save }, 'Сохранить'),
        h('button', { type: 'button', class: 'small', onclick: cancel }, 'Отмена')));
    queueMicrotask(() => { if (document.activeElement !== ta) ta.focus(); });
    return el;
  }

  // --- подключение ---

  function connect() {
    const es = new EventSource(`/api/boards/${id}/events?participant=${participant}`);
    es.addEventListener('board', e => {
      const data = JSON.parse(e.data);
      online = data.online;
      board = data;
      setStatus('');
      render();
    });
    es.onopen = () => setStatus('');
    es.onerror = () => setStatus('Нет связи с сервером, переподключение…');
  }

  (async () => {
    try {
      board = await api('GET', `/api/boards/${id}`);
      rememberBoard(board.id, board.title);
      render();
      connect();
    } catch (err) {
      const missing = err.status === 404;
      root.replaceChildren(h('div', { class: 'notfound' },
        h('h1', {}, missing ? 'Доска не найдена' : 'Не удалось открыть доску'),
        h('p', {}, missing ? 'Проверьте ссылку: возможно, доска была удалена.' : err.message),
        h('a', { href: '/', class: 'button' }, 'На главную')));
    }
  })();
}

// ---------- Маршрутизация ----------

document.addEventListener('DOMContentLoaded', () => {
  const root = document.getElementById('app');
  const m = location.pathname.match(/^\/b\/([A-Za-z0-9]+)\/?$/);
  if (m) renderBoardPage(root, m[1]);
  else renderHome(root);
});
