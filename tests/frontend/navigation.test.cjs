const { test: nodeTest, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { asset } = require('./assets.cjs');
const { chromium } = require('playwright');
const test = (name, fn) => nodeTest(name, { timeout: 20000 }, fn);
const origin = 'http://retro.test';
const participant = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const storageKey = `retro.drafts.v1:board1234:${participant}`;
const field = (page, key) => page.locator(`[data-keep="${key}"]`);
const recovery = page => page.getByRole('region', { name: 'Неотправленные записи' });
let browser;
before(async () => {
  browser = await chromium.launch({ headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}),
  });
});
after(async () => { await browser?.close(); });

async function openBoard(t) {
  const context = await browser.newContext();
  t.after(() => context.close());
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  let board = {
    id: 'board1234', title: 'Доска', revision: 1, phase: 'reveal', isOwner: true,
    voteLimit: 3, votesLeft: 3, online: 1,
    columns: [{ id: 'col1', title: 'Хорошо', cards: [
      { id: 'card1', text: 'Карточка', author: 'Анна', mine: true, revision: 1 },
    ] }],
    actions: [{ id: 'action1', text: 'Решение', assignee: 'Анна', mine: true, done: false, revision: 1 }],
  };
  await page.addInitScript(() => {

    window.streams = [];
    window.EventSource = class {
      constructor() { this.listeners = {}; window.streams.push(this); }
      addEventListener(name, listener) { this.listeners[name] = listener; }
      close() { this.closed = true; }
    };
    const fetch = window.fetch;
    window.requests = [];
    window.fetch = (url, options) => {
      if ((options.method !== 'GET' && window.holdMutations) || (options.method === 'GET' && window.holdGet)) {
        const request = { method: options.method, path: String(url), signal: options.signal,
          body: options.body && JSON.parse(options.body), key: options.headers['Idempotency-Key'] };
        window.requests.push(request);
        // Намеренно игнорируем abort у транспорта: сам api обязан завершить
        // ожидание и не принимать запоздалый результат после pagehide.
        return new Promise(resolve => { request.finish = body => resolve(new Response(JSON.stringify(body), {
          status: 200, headers: { 'Content-Type': 'application/json' },
        })); });
      }
      return fetch(url, options);
    };
  });
  await page.route(`${origin}/**`, route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/boards/board1234') return route.fulfill({ json: board });
    const name = url.pathname === '/static/app.js' ? 'app.js' : url.pathname === '/static/style.css' ? 'style.css' : 'index.html';
    return route.fulfill({ contentType: name.endsWith('.js') ? 'application/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html',
      body: asset(name) });
  });
  await page.goto(`${origin}/b/board1234`);
  await field(page, 'draft:col1').waitFor();
  return { page, get board() { return structuredClone(board); }, set board(value) { board = structuredClone(value); } };
}
async function snapshot(page) {
  return page.evaluate(key => JSON.parse(sessionStorage.getItem(key)), storageKey);
}
async function resume(page, raw) {
  // Здесь проверяется обработчик жизненного цикла с точным порядком
  // callback'ов; настоящий BFCache и нативный SSE проверяет live integration.
  await page.evaluate(({ key, raw }) => {
    dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
    if (raw === null) sessionStorage.removeItem(key);
    else sessionStorage.setItem(key, raw);
    dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
  }, { key: storageKey, raw });
  await field(page, 'draft:col1').waitFor();
}
async function edit(page, kind) {
  await page.locator(kind === 'card' ? '.card.mine' : '.action').getByRole('button', { name: 'Изменить', exact: true }).click();
}
async function holdCreation(page, text) {
  await page.evaluate(() => { window.holdMutations = true; });
  await field(page, 'draft:col1').fill(text);
  await page.getByRole('button', { name: 'Добавить', exact: true }).click();
  await page.waitForFunction(() => window.requests.length > 0);
}

test('Восстановление страницы заменяет все черновики и очередь актуальным снимком вкладки', async t => {
  const { page } = await openBoard(t);
  await edit(page, 'card');
  await field(page, 'edit:card1').fill('Старый редактор карточки');
  await edit(page, 'action');
  await field(page, 'action-edit:action1').fill('Старый редактор решения');
  await holdCreation(page, 'Старая отправка');
  await field(page, 'draft:col1').fill('Старый следующий черновик');
  const latest = await snapshot(page);
  latest.drafts.col1 = 'Последний черновик';
  latest.actionDraft = { text: 'Последнее решение', assignee: 'Борис' };
  latest.cardEdits = [];
  latest.editingId = null;
  latest.actionEdits[0][1].text = 'Последние правки решения';
  latest.actionEdits[0][1].assignee = 'Вера';
  latest.outbox = [{ path: '/api/boards/board1234/actions',
    body: { text: 'Новая неотправленная запись', assignee: 'Денис' },
    options: { idempotencyKey: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' }, error: 'Ошибка новой отправки' }];
  await resume(page, JSON.stringify(latest));
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Последний черновик');
  assert.equal(await field(page, 'action-text').inputValue(), 'Последнее решение');
  assert.equal(await field(page, 'action-assignee').inputValue(), 'Борис');
  assert.equal(await field(page, 'edit:card1').count(), 0);
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Последние правки решения');
  assert.equal(await field(page, 'action-edit-assignee:action1').inputValue(), 'Вера');
  await recovery(page).getByText('Новая неотправленная запись', { exact: true }).waitFor();
  assert.equal(await recovery(page).getByText('Старая отправка', { exact: true }).count(), 0);
  assert.equal(await page.evaluate(() => window.requests[0].signal.aborted), true);
  await field(page, 'draft:col1').fill('Последний черновик +');
  assert.deepEqual((await snapshot(page)).outbox, latest.outbox);
});

test('Удалённый снимок не возрождает старые редакторы и очередь после возврата', async t => {
  const { page } = await openBoard(t);
  await edit(page, 'card');
  await field(page, 'edit:card1').fill('Уже отменённые правки');
  await holdCreation(page, 'Уже отправленная запись');
  await field(page, 'draft:col1').fill('Уже очищенный черновик');
  await resume(page, null);
  assert.equal(await field(page, 'draft:col1').inputValue(), '');
  assert.equal(await field(page, 'edit:card1').count(), 0);
  assert.equal(await recovery(page).getByRole('button').count(), 0);
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), null);
  await field(page, 'draft:col1').fill('Новая запись');
  assert.deepEqual((await snapshot(page)).outbox, []);
});

test('Неподдерживаемый актуальный снимок при возврате остаётся нетронутым', async t => {
  const { page } = await openBoard(t);
  await field(page, 'draft:col1').fill('Доступный старый текст');
  const raw = '{"version":999,"drafts":{"col1":"Более новые данные"}}';
  await resume(page, raw);
  await page.locator('.draft-warning').getByText(/Не удалось восстановить/).waitFor();
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Доступный старый текст');
  await field(page, 'draft:col1').fill('Текущий текст');
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), raw);
});

test('Недоступное чтение при возврате сохраняет текст в памяти и не затирает неизвестный снимок', async t => {
  const { page } = await openBoard(t);
  await field(page, 'draft:col1').fill('Текст в памяти');
  const raw = JSON.stringify({ ...await snapshot(page), drafts: { col1: 'Более новый текст в хранилище' } });
  await page.evaluate(({ key, raw }) => {
    dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
    sessionStorage.setItem(key, raw);
    const getItem = Storage.prototype.getItem;
    window.readRaw = () => getItem.call(sessionStorage, key);
    Storage.prototype.getItem = function (key) {
      if (this === sessionStorage && key.startsWith('retro.drafts.')) throw new DOMException('Blocked', 'SecurityError');
      return getItem.call(this, key);
    };
    dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
  }, { key: storageKey, raw });
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Текст в памяти');
  await field(page, 'draft:col1').fill('Новый ввод в памяти');
  await page.locator('.draft-warning').getByText(/Браузер не сохраняет черновики/).waitFor();
  assert.equal(await page.evaluate(() => window.readRaw()), raw);
});

test('Отменённый старый POST не удаляет восстановленную очередь и не снимает pending нового повтора', async t => {
  const env = await openBoard(t);
  const { page } = env;
  await holdCreation(page, 'Сохранить исходный ключ');
  const saved = await snapshot(page);
  await resume(page, JSON.stringify(saved));
  const retry = recovery(page).getByRole('button', { name: 'Повторить отправку', exact: true });
  await retry.click();
  await page.waitForFunction(() => window.requests.length === 2);
  const requests = await page.evaluate(() => window.requests.map(r => ({ body: r.body, key: r.key, aborted: r.signal.aborted })));
  assert.deepEqual(requests.map(r => r.key), [saved.outbox[0].options.idempotencyKey, saved.outbox[0].options.idempotencyKey]);
  assert.deepEqual(requests[0].body, requests[1].body);
  assert.equal(requests[0].aborted, true);
  await page.evaluate(board => window.requests[0].finish({ ...board, revision: 99 }), env.board);
  await page.waitForTimeout(50);
  assert.equal(await retry.isDisabled(), true);
  assert.equal((await snapshot(page)).outbox.length, 1);
  const confirmed = env.board;
  confirmed.revision = 2;
  confirmed.columns[0].cards.push({ id: 'card2', text: 'Сохранить исходный ключ', mine: true, revision: 1 });
  await page.evaluate(board => window.requests[1].finish(board), confirmed);
  await recovery(page).getByText('Сохранить исходный ключ', { exact: true }).waitFor({ state: 'detached' });
  await page.locator('.card').getByText('Сохранить исходный ключ', { exact: true }).waitFor();
});

test('Незавершённый PATCH после возврата сохраняет исходную revision и ждёт свежего сравнения', async t => {
  const env = await openBoard(t);
  const { page } = env;
  await edit(page, 'action');
  await field(page, 'action-edit:action1').fill('Мой текст');
  await page.evaluate(() => { window.holdMutations = true; });
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  await page.waitForFunction(() => window.requests.length === 1);
  const saved = await snapshot(page);
  assert.equal(saved.actionEdits[0][1].uncertain, true);
  const current = env.board;
  current.revision = 2;
  current.actions[0] = { ...current.actions[0], revision: 2, text: 'Текст сервера', assignee: 'Борис' };
  env.board = current;
  await resume(page, JSON.stringify(saved));
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Мой текст');
  assert.equal(await field(page, 'action-edit:action1').isEnabled(), true);
  assert.equal(await page.getByRole('button', { name: 'Сохранить', exact: true }).isDisabled(), true);
  await page.locator('.edit-conflict').getByText('Текст сервера', { exact: true }).waitFor();
  await page.evaluate(board => window.requests[0].finish({ ...board, revision: 99 }), env.board);
  await page.waitForTimeout(50);
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Мой текст');
  assert.equal((await snapshot(page)).actionEdits[0][1].baseRevision, 1);
  await page.getByRole('button', { name: 'Загрузить актуальную версию', exact: true }).click();
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Текст сервера');
  assert.equal((await snapshot(page)).actionEdits[0][1].baseRevision, 2);
});

test('Quota при неизменившемся storage не удаляет единственный черновик из памяти на возврате', async t => {
  const { page } = await openBoard(t);
  await page.evaluate(() => {
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (this === sessionStorage && key.startsWith('retro.drafts.')) throw new DOMException('Quota', 'QuotaExceededError');
      return setItem.call(this, key, value);
    };
  });
  await field(page, 'draft:col1').fill('Единственная копия в памяти');
  await edit(page, 'action');
  await field(page, 'action-edit:action1').fill('Правки только в памяти');
  assert.equal(await snapshot(page), null);
  await resume(page, null);
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Единственная копия в памяти');
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Правки только в памяти');
  await page.locator('.draft-warning').getByText(/Браузер не сохраняет черновики/).waitFor();
  assert.equal(await snapshot(page), null);
});

test('Quota и другая сохранённая версия требуют явного выбора без потери обеих копий', async t => {
  const { page } = await openBoard(t);
  await field(page, 'draft:col1').fill('Сохранённая исходная копия');
  const newer = { ...await snapshot(page), drafts: { col1: 'Более новый сохранённый текст' } };
  await page.evaluate(() => {
    const setItem = Storage.prototype.setItem;
    window.writeStored = (key, value) => setItem.call(sessionStorage, key, value);
    Storage.prototype.setItem = function (key, value) {
      if (this === sessionStorage && key.startsWith('retro.drafts.')) throw new DOMException('Quota', 'QuotaExceededError');
      return setItem.call(this, key, value);
    };
  });
  await field(page, 'draft:col1').fill('Несохранённый текст в памяти');
  await holdCreation(page, 'Неотправленная запись только в памяти');
  await field(page, 'draft:col1').fill('Несохранённый следующий текст');
  const raw = JSON.stringify(newer);
  await page.evaluate(({ key, raw }) => {
    dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
    window.writeStored(key, raw);
    dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
  }, { key: storageKey, raw });
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Несохранённый следующий текст');
  await recovery(page).getByText('Неотправленная запись только в памяти', { exact: true }).waitFor();
  const load = page.locator('.draft-warning').getByRole('button', { name: 'Загрузить сохранённые черновики', exact: true });
  await load.waitFor();
  await field(page, 'draft:col1').fill('Ещё один ввод в памяти');
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), raw);
  page.once('dialog', dialog => dialog.dismiss());
  await load.click();
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Ещё один ввод в памяти');
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), raw);
  page.once('dialog', dialog => dialog.accept());
  await load.click();
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Более новый сохранённый текст');
  assert.equal(await recovery(page).getByRole('button').count(), 0);
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), raw);
});

test('Текст, набранный при недоступном чтении, сохраняется после восстановления storage и нового возврата', async t => {
  const { page } = await openBoard(t);
  await field(page, 'draft:col1').fill('Исходный сохранённый текст');
  await page.evaluate(() => {
    dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
    const getItem = Storage.prototype.getItem;
    window.restoreStorageRead = () => { Storage.prototype.getItem = getItem; };
    Storage.prototype.getItem = function (key) {
      if (this === sessionStorage && key.startsWith('retro.drafts.')) throw new DOMException('Blocked', 'SecurityError');
      return getItem.call(this, key);
    };
    dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
  });
  await field(page, 'draft:col1').fill('Новый текст только в памяти');
  await page.evaluate(() => {
    dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
    window.restoreStorageRead();
    dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
  });
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Новый текст только в памяти');
  assert.equal((await snapshot(page)).drafts.col1, 'Новый текст только в памяти');
  assert.equal(await page.locator('.draft-warning').isVisible(), false);
});
