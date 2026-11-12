const { test: nodeTest, before, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const test = (name, fn) => nodeTest(name, { timeout: 20000 }, fn);
const origin = 'http://retro.test';
const webDir = path.resolve(__dirname, '../../web');
const field = (page, key) => page.locator(`[data-keep="${key}"]`);
const warning = page => page.locator('[data-ui="identity-warning"]');
const localID = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const tabID = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';
let browser;

before(async () => {
  browser = await chromium.launch({ headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}),
  });
});
after(async () => { await browser?.close(); });

async function openPage(t, options = {}) {
  const page = await browser.newPage();
  t.after(() => page.close());
  page.setDefaultTimeout(5000);
  const reads = [];
  const creations = [];
  const board = {
    id: 'board1234', title: 'Доска участника', revision: 1, phase: 'reveal', isOwner: true,
    voteLimit: 3, votesLeft: 3, online: 1,
    columns: [{ id: 'col1', title: 'Хорошо', cards: [] }], actions: [],
  };
  let creationReply = { status: 503, json: { error: 'Ответ на создание потерян' } };
  await page.addInitScript(initial => {
    const local = window.localStorage;
    const session = window.sessionStorage;
    const get = Storage.prototype.getItem;
    const set = Storage.prototype.setItem;
    const controlKey = 'test.identity.storage-mode';
    if (!get.call(session, controlKey)) {
      if (initial.localID !== undefined) set.call(local, 'retro.participant', initial.localID);
      if (initial.sessionID !== undefined) set.call(session, 'retro.participant', initial.sessionID);
      set.call(session, controlKey, JSON.stringify({ local: initial.local || 'ok', session: initial.session || 'ok' }));
    }
    const modes = JSON.parse(get.call(session, controlKey));
    const mode = storage => storage === local ? modes.local : storage === session ? modes.session : 'ok';
    Storage.prototype.getItem = function (key) {
      if (mode(this) === 'read-blocked') throw new DOMException('Доступ запрещён', 'SecurityError');
      return get.call(this, key);
    };
    Storage.prototype.setItem = function (key, value) {
      if (mode(this) === 'quota') throw new DOMException('Нет места', 'QuotaExceededError');
      if (mode(this) === 'silent') return;
      return set.call(this, key, value);
    };
    for (const [name, storage] of [['localStorage', local], ['sessionStorage', session]]) {
      if (mode(storage) === 'blocked') Object.defineProperty(window, name, {
        get() { throw new DOMException('Доступ запрещён', 'SecurityError'); },
      });
    }
    // Меняем доступность хранилища между загрузками, а не состояние приложения.
    window.identityTestConfigure = change => set.call(session, controlKey, JSON.stringify({ ...modes, ...change }));
    window.EventSource = class {
      constructor(url) { window.testEvents = this; window.testEventsURL = url; this.listeners = {}; }
      addEventListener(name, listener) { this.listeners[name] = listener; }
      close() {}
    };
  }, options);
  await page.route(`${origin}/**`, route => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/boards/board1234') {
      reads.push(request.headers()['x-participant']);
      return route.fulfill({ json: board });
    }
    if (pathname === '/api/boards/board1234/cards') {
      creations.push({ participant: request.headers()['x-participant'], key: request.headers()['idempotency-key'], body: request.postDataJSON() });
      return route.fulfill(creationReply);
    }
    if (pathname === '/api/templates') return route.fulfill({ json: [
      { id: 'classic', name: 'Классика', columns: ['Хорошо'] },
    ] });
    if (pathname === '/api/boards/board1234/history') return route.fulfill({ json: [] });
    const asset = pathname === '/static/app.js' ? 'app.js' : pathname === '/static/style.css' ? 'style.css' : null;
    if (asset || pathname === '/' || pathname === '/b/board1234') return route.fulfill({
      contentType: asset === 'app.js' ? 'application/javascript' : asset === 'style.css' ? 'text/css' : 'text/html',
      body: fs.readFileSync(path.join(webDir, asset || 'index.html'), 'utf8'),
    });
    return route.fulfill({ status: 404, json: { error: 'Неожиданный запрос' } });
  });
  await page.goto(`${origin}${options.home ? '/' : '/b/board1234'}`);
  if (!options.home) await field(page, 'draft:col1').waitFor();
  return {
    page, reads, creations, board,
    confirmCreations() { creationReply = { json: { ...board, revision: 2 } }; },
    async reload() { await page.reload(); await field(page, 'draft:col1').waitFor(); },
    async emit() { await page.evaluate(data => window.testEvents.listeners.board({ data: JSON.stringify(data) }), board); },
  };
}

for (const fault of ['quota', 'read-blocked', 'blocked', 'silent']) {
  test(`localStorage ${fault}: F5 сохраняет участника, новый черновик и ключ ручного повтора POST`, async t => {
    const env = await openPage(t, { local: fault });
    const { page } = env;
    const participant = env.reads.at(-1);
    assert.match(participant, /^[a-f0-9]{32}$/);
    await warning(page).waitFor();
    assert.match(await warning(page).textContent(), /только в этой вкладке/);
    await field(page, 'draft:col1').fill('Не подтверждённая карточка');
    await page.getByRole('button', { name: 'Добавить', exact: true }).click();
    const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
    await recovery.getByText('Ответ на создание потерян', { exact: true }).waitFor();
    const sent = { ...env.creations[0] };
    await field(page, 'draft:col1').fill('Следующая карточка');
    await field(page, 'action-text').fill('Черновик решения');
    await env.reload();
    assert.equal(env.reads.at(-1), participant);
    assert.equal(await field(page, 'draft:col1').inputValue(), 'Следующая карточка');
    assert.equal(await field(page, 'action-text').inputValue(), 'Черновик решения');
    await recovery.getByText('Не подтверждённая карточка', { exact: true }).waitFor();
    assert.equal(env.creations.length, 1, 'Восстановление не должно автоматически повторять POST');
    const sseParticipant = await page.evaluate(() => new URL(window.testEventsURL, location.href).searchParams.get('participant'));
    assert.equal(sseParticipant, participant);
    env.confirmCreations();
    await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
    await recovery.getByText('Не подтверждённая карточка', { exact: true }).waitFor({ state: 'detached' });
    assert.deepEqual(env.creations[1], sent, 'Повтор обязан сохранить participant, idempotency key и исходный текст');
    assert.equal(await field(page, 'draft:col1').inputValue(), 'Следующая карточка');
  });
}

test('Существующий валидный local ID сохраняет права при недоступном sessionStorage', async t => {
  const env = await openPage(t, { localID, session: 'blocked' });
  assert.equal(env.reads.at(-1), localID);
  assert.equal(await warning(env.page).isVisible(), false);
  await env.reload();
  assert.equal(env.reads.at(-1), localID);
  assert.equal(await warning(env.page).isVisible(), false);
});

test('Восстановление записи localStorage не меняет session-участника и не теряет его черновик', async t => {
  const env = await openPage(t, { local: 'quota' });
  const participant = env.reads.at(-1);
  await field(env.page, 'draft:col1').fill('Черновик временного участника');
  await env.page.evaluate(() => window.identityTestConfigure({ local: 'ok' }));
  await env.reload();
  assert.equal(env.reads.at(-1), participant);
  assert.equal(await field(env.page, 'draft:col1').inputValue(), 'Черновик временного участника');
  assert.equal(await warning(env.page).isVisible(), false);
  await env.reload();
  assert.equal(env.reads.at(-1), participant);
});

test('Ошибка чтения localStorage не затирает прежний ID слепой записью нового участника', async t => {
  const env = await openPage(t, { localID, local: 'read-blocked' });
  const fallback = env.reads.at(-1);
  assert.match(fallback, /^[a-f0-9]{32}$/);
  assert.notEqual(fallback, localID);
  await field(env.page, 'draft:col1').fill('Черновик временной вкладки');
  await env.page.evaluate(() => window.identityTestConfigure({ local: 'ok' }));
  await env.reload();
  assert.equal(env.reads.at(-1), fallback, 'Вкладка продолжает свою прежнюю сессию');
  assert.equal(await field(env.page, 'draft:col1').inputValue(), 'Черновик временной вкладки');
  assert.equal(await env.page.evaluate(() => localStorage.getItem('retro.participant')), localID,
    'Недоступность чтения не разрешает затирать участника, сохранённого для остальных вкладок');
});

test('Вкладка сохраняет свой session ID и не перезаписывает другого валидного local-участника', async t => {
  const env = await openPage(t, { localID, sessionID: tabID });
  assert.equal(env.reads.at(-1), tabID);
  assert.equal(await env.page.evaluate(() => localStorage.getItem('retro.participant')), localID);
  assert.match(await warning(env.page).textContent(), /только в этой вкладке/);
  await field(env.page, 'draft:col1').fill('Черновик этой вкладки');
  await env.reload();
  assert.equal(env.reads.at(-1), tabID);
  assert.equal(await field(env.page, 'draft:col1').inputValue(), 'Черновик этой вкладки');
  assert.equal(await env.page.evaluate(() => localStorage.getItem('retro.participant')), localID);
});

for (const sessionFault of ['blocked', 'silent']) {
  test(`Оба хранилища недоступны (${sessionFault}): постоянное предупреждение на главной и доске`, async t => {
    const env = await openPage(t, { local: 'blocked', session: sessionFault, home: true });
    const { page } = env;
    await warning(page).waitFor();
    assert.match(await warning(page).textContent(), /не сохраняет идентификатор участника/);
    assert.match(await warning(page).textContent(), /После обновления или закрытия/);
    assert.doesNotMatch(await warning(page).textContent(), /сохранён только/);
    await page.goto(`${origin}/b/board1234`);
    await field(page, 'draft:col1').waitFor();
    assert.equal(await warning(page).isVisible(), true);
    await field(page, 'draft:col1').fill('Текущий ввод доступен');
    await env.emit();
    assert.equal(await field(page, 'draft:col1').inputValue(), 'Текущий ввод доступен');
    assert.equal(await warning(page).isVisible(), true, 'Перерисовка доски не должна убирать предупреждение');
    assert.match(await warning(page).textContent(), /не сохраняет идентификатор участника/);
    await page.goto(`${origin}/`);
    assert.equal(await warning(page).isVisible(), true);
  });
}

test('Невалидные ID из обоих хранилищ не попадают в HTTP и SSE', async t => {
  const env = await openPage(t, { localID: 'не участник', sessionID: '../board-owner' });
  const participant = env.reads.at(-1);
  assert.match(participant, /^[a-f0-9]{32}$/);
  assert.notEqual(participant, 'не участник');
  assert.notEqual(participant, '../board-owner');
  assert.equal(await warning(env.page).isVisible(), false);
  const sseParticipant = await env.page.evaluate(() => new URL(window.testEventsURL, location.href).searchParams.get('participant'));
  assert.equal(sseParticipant, participant);
  await env.reload();
  assert.equal(env.reads.at(-1), participant);
});

test('Невалидный session ID не вытесняет существующего валидного local-участника', async t => {
  const env = await openPage(t, { localID, sessionID: 'invalid-session' });
  assert.equal(env.reads.at(-1), localID);
  await env.reload();
  assert.equal(env.reads.at(-1), localID);
});
