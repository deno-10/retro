const { test: nodeTest, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { asset } = require('./assets.cjs');
const { chromium } = require('playwright');
const test = (name, fn) => nodeTest(name, { timeout: 20000 }, fn);
const origin = 'http://retro.test';
const participant = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const storageKey = `retro.drafts.v1:board1234:${participant}`;
const clone = value => structuredClone(value);
const field = (page, key) => page.locator(`[data-keep="${key}"]`);
let browser;
before(async () => {
  browser = await chromium.launch({ headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}),
  });
});
after(async () => { await browser?.close(); });

async function openBoard(t, options = {}) {
  const context = options.context || await browser.newContext();
  if (!options.context) t.after(() => context.close());
  const page = await context.newPage();
  if (options.context) t.after(() => page.close());
  page.setDefaultTimeout(5000);
  await page.clock.install();
  let board = options.board || {
    id: 'board1234', title: 'Доска', revision: 1, phase: 'reveal', isOwner: true,
    voteLimit: 3, votesLeft: 3, online: 1,
    columns: [{ id: 'col1', title: 'Хорошо', cards: [
      { id: 'card1', text: 'Карточка', author: 'Анна', mine: true, revision: 1 },
    ] }],
    actions: [{ id: 'action1', text: 'Решение', assignee: 'Анна', mine: true, done: false, revision: 1 }],
  };
  await page.addInitScript(({ storageKey, fault, bodyHang }) => {
    if (fault === 'invalid') sessionStorage.setItem(storageKey, '{"version":999}');
    if (fault === 'quota') {
      const setItem = Storage.prototype.setItem;
      Storage.prototype.setItem = function (...args) {
        if (this === sessionStorage) throw new DOMException('Quota', 'QuotaExceededError');
        return setItem.apply(this, args);
      };
    }
    if (fault === 'blocked') Object.defineProperty(window, 'sessionStorage', {
      get() { throw new DOMException('Blocked', 'SecurityError'); },
    });
    window.EventSource = class {
      constructor() { window.testEvents = this; this.listeners = {}; }
      addEventListener(name, listener) { this.listeners[name] = listener; }
      close() {}
    };
    if (bodyHang) {
      const fetch = window.fetch;
      window.fetch = async (url, options) => {
        if (!window.hungBody && String(url).endsWith(bodyHang.path) && options.method === bodyHang.method) {
          window.hungBody = { body: JSON.parse(options.body), key: options.headers['Idempotency-Key'] };
          return new Response(new ReadableStream({
            start(controller) {
              options.signal.addEventListener('abort', () => controller.error(new DOMException('Aborted', 'AbortError')), { once: true });
            },
          }), { status: 200, headers: { 'Content-Type': 'application/json' } });
        }
        return fetch(url, options);
      };
    }
  }, { storageKey, fault: options.fault, bodyHang: options.bodyHang });
  let viewer = participant;
  await page.route(`${origin}/**`, route => {
    const url = new URL(route.request().url());
    const match = url.pathname.match(/^\/api\/boards\/([A-Za-z0-9]+)$/);
    if (match) return route.fulfill({ json: { ...board, id: match[1] } });
    const name = url.pathname === '/static/app.js' ? 'app.js' : url.pathname === '/static/style.css' ? 'style.css' : 'index.html';
    return route.fulfill({ contentType: name.endsWith('.js') ? 'application/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html',
      body: asset(name, viewer) });
  });
  await page.goto(`${origin}/b/board1234`);
  await page.locator('.board-title').waitFor();
  return {
    page, context,
    setViewer(id) { viewer = id; },
    get board() { return clone(board); },
    set board(value) { board = clone(value); },
    emit(value) { return page.evaluate(data => window.testEvents.listeners.board({ data: JSON.stringify({ ...data, participant: document.querySelector('meta[name="retro-participant"]').content }) }), value); },
  };
}
async function snapshot(page) {
  return page.evaluate(key => JSON.parse(sessionStorage.getItem(key)), storageKey);
}
async function edit(page, kind) {
  await page.locator(kind === 'card' ? '.card.mine' : '.action').getByRole('button', { name: 'Изменить', exact: true }).click();
}

for (const kind of ['cards', 'actions']) {
  test(`Незавершённый POST ${kind} сохраняется до fetch и после F5 повторяется вручную с прежним ключом`, async t => {
    const env = await openBoard(t);
    const { page } = env;
    const draftKey = kind === 'cards' ? 'draft:col1' : 'action-text';
    const button = kind === 'cards' ? 'Добавить' : 'Записать';
    let start;
    const started = new Promise(resolve => { start = resolve; });
    let finish;
    const held = new Promise(resolve => { finish = resolve; });
    await page.route(`${origin}/api/boards/board1234/${kind}`, async route => {
      start({ body: route.request().postDataJSON(), key: route.request().headers()['idempotency-key'] });
      await held;
      await route.abort().catch(() => {});
    }, { times: 1 });
    await field(page, draftKey).fill('Отправляемая запись');
    await page.getByRole('button', { name: button, exact: true }).click();
    const original = await started;
    const pending = await snapshot(page);
    assert.equal(pending.outbox[0].options.idempotencyKey, original.key);
    assert.deepEqual(pending.outbox[0].body, original.body);
    await field(page, draftKey).fill('Следующий черновик');
    await page.reload();
    finish();
    const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
    await recovery.getByText('Отправляемая запись', { exact: true }).waitFor();
    assert.equal(await field(page, draftKey).inputValue(), 'Следующий черновик');
    let repeated;
    await page.route(`${origin}/api/boards/board1234/${kind}`, route => {
      repeated = { body: route.request().postDataJSON(), key: route.request().headers()['idempotency-key'] };
      return route.fulfill({ json: { ...env.board, revision: 2 } });
    }, { times: 1 });
    assert.equal(repeated, undefined, 'После восстановления нельзя автоматически отправлять запись');
    await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
    await recovery.getByText('Отправляемая запись', { exact: true }).waitFor({ state: 'detached' });
    assert.deepEqual(repeated, original);
    assert.equal((await snapshot(page)).outbox.length, 0);
  });
}

test('Черновики разделены по доске, участнику и вкладке', async t => {
  const env = await openBoard(t);
  const { page } = env;
  await field(page, 'draft:col1').fill('Доска A');
  await page.goto(`${origin}/b/board9999`);
  assert.equal(await field(page, 'draft:col1').inputValue(), '');
  await field(page, 'draft:col1').fill('Доска B');
  await page.goto(`${origin}/b/board1234`);
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Доска A');
  env.setViewer('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb');
  await page.reload();
  assert.equal(await field(page, 'draft:col1').inputValue(), '');
  env.setViewer(participant);
  await page.reload();
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Доска A');
  const other = await openBoard(t, { context: env.context });
  assert.equal(await field(other.page, 'draft:col1').inputValue(), '');
  await field(other.page, 'draft:col1').fill('Другая вкладка');
  await page.reload();
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Доска A');
});

for (const fault of ['blocked', 'quota', 'invalid']) {
  test(`Ошибка sessionStorage ${fault} заметна и не мешает текущему вводу`, async t => {
    const { page } = await openBoard(t, { fault });
    if (fault === 'invalid') assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), '{"version":999}');
    await field(page, 'draft:col1').fill('Не терять текущий ввод');
    const warning = page.locator('.draft-warning');
    await warning.waitFor();
    assert.match(await warning.textContent(), fault === 'invalid' ? /Не удалось восстановить/ : /Браузер не сохраняет черновики/);
    assert.equal(await field(page, 'draft:col1').inputValue(), 'Не терять текущий ввод');
    if (fault === 'invalid') {
      assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), '{"version":999}');
      page.once('dialog', dialog => dialog.dismiss());
      await warning.getByRole('button', { name: 'Сбросить сохранённые черновики', exact: true }).click();
      assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), '{"version":999}');
      assert.equal(await field(page, 'draft:col1').inputValue(), 'Не терять текущий ввод');
      page.once('dialog', dialog => dialog.accept());
      await warning.getByRole('button', { name: 'Сбросить сохранённые черновики', exact: true }).click();
      assert.equal((await snapshot(page)).drafts.col1, 'Не терять текущий ввод');
      assert.equal(await warning.isVisible(), false);
    }
  });
}

test('Допустимые сервером Unicode тексты редакторов переживают F5 без отклонения снимка', async t => {
  const env = await openBoard(t);
  const data = env.board;
  data.columns[0].cards[0].text = '😀'.repeat(2000);
  data.actions[0].assignee = '😀'.repeat(60);
  env.board = data;
  await env.emit(data);
  await edit(env.page, 'card');
  await edit(env.page, 'action');
  await env.page.reload();
  assert.equal(await field(env.page, 'edit:card1').inputValue(), data.columns[0].cards[0].text);
  assert.equal(await field(env.page, 'action-edit-assignee:action1').inputValue(), data.actions[0].assignee);
  assert.equal(await env.page.locator('.draft-warning').isVisible(), false);
});

test('Зависшее чтение JSON ответа POST ограничено 15 секундами, повтор сохраняет ключ и новый черновик', async t => {
  const env = await openBoard(t, { bodyHang: { method: 'POST', path: '/cards' } });
  const { page } = env;
  await field(page, 'draft:col1').fill('Запись с зависшим JSON');
  await page.getByRole('button', { name: 'Добавить', exact: true }).click();
  await page.waitForFunction(() => !!window.hungBody);
  const sent = await page.evaluate(() => window.hungBody);
  await field(page, 'draft:col1').fill('Новый ввод во время запроса');
  await page.clock.fastForward(15001);
  const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
  await recovery.getByText(/Сервер не ответил за 15 секунд/).waitFor();
  assert.equal(await page.getByRole('button', { name: 'Добавить', exact: true }).isEnabled(), true);
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Новый ввод во время запроса');
  let key;
  await page.route(`${origin}/api/boards/board1234/cards`, route => {
    key = route.request().headers()['idempotency-key'];
    return route.fulfill({ json: { ...env.board, revision: 2 } });
  }, { times: 1 });
  await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
  await recovery.getByText('Запись с зависшим JSON', { exact: true }).waitFor({ state: 'detached' });
  assert.equal(key, sent.key);
});

test('PATCH с зависшим JSON разблокирует редактор и сверяет сервер, сохраняя исходную revision после F5', async t => {
  const env = await openBoard(t, { bodyHang: { method: 'PATCH', path: '/actions/action1' } });
  const { page } = env;
  await edit(page, 'action');
  await field(page, 'action-edit:action1').fill('Уже сохранённый сервером текст');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  await page.waitForFunction(() => !!window.hungBody);
  const committed = env.board;
  committed.revision = 2;
  committed.actions[0] = { ...committed.actions[0], text: 'Уже сохранённый сервером текст', revision: 2 };
  env.board = committed;
  await page.clock.fastForward(15001);
  await page.getByText('Не удалось подтвердить сохранение', { exact: true }).waitFor();
  await page.locator('.edit-conflict').getByText('Уже сохранённый сервером текст', { exact: true }).waitFor();
  assert.equal(await field(page, 'action-edit:action1').isEnabled(), true);
  assert.equal(await page.getByRole('button', { name: 'Отмена', exact: true }).isEnabled(), true);
  assert.equal(await page.getByRole('button', { name: 'Сохранить', exact: true }).isDisabled(), true);
  assert.equal((await snapshot(page)).actionEdits[0][1].baseRevision, 1);
  await page.reload();
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Уже сохранённый сервером текст');
  assert.equal((await snapshot(page)).actionEdits[0][1].baseRevision, 1);
  assert.equal(await page.getByRole('button', { name: 'Сохранить', exact: true }).isDisabled(), true);
  await page.getByRole('button', { name: 'Отмена', exact: true }).click();
  await field(page, 'action-edit:action1').waitFor({ state: 'detached' });
  await page.locator('.action').getByText('Уже сохранённый сервером текст', { exact: true }).waitFor();
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), storageKey), null);
});

test('Голос группы отправляет true/false, остаётся заблокированным при смене первой карточки и откатывается при ошибке', async t => {
  const env = await openBoard(t);
  const { page } = env;
  const voting = env.board;
  voting.phase = 'vote';
  voting.columns[0].cards[0] = { ...voting.columns[0].cards[0], groupId: 'group1', groupVoted: false, groupVotes: 0 };
  voting.columns[0].cards.push({ id: 'card2', text: 'Вторая', groupId: 'group1', groupVoted: false, groupVotes: 0, revision: 1 });
  await env.emit(voting);
  let release, started;
  const request = new Promise(resolve => { started = resolve; });
  const response = new Promise(resolve => { release = resolve; });
  await page.route(`${origin}/api/boards/board1234/cards/card1/vote`, async route => {
    started(route.request().postDataJSON());
    await response;
    await route.fulfill({ status: 503, json: { error: 'Голос не подтверждён' } });
  }, { times: 1 });
  await page.locator('button.vote').click();
  assert.deepEqual(await request, { voted: true });
  voting.columns[0].cards.reverse();
  await env.emit({ ...voting, revision: 2 });
  assert.equal(await page.locator('button.vote').isDisabled(), true);
  assert.equal(await page.locator('button.vote').getAttribute('aria-pressed'), 'true');
  release();
  await page.getByText('Голос не подтверждён', { exact: true }).waitFor();
  assert.equal(await page.locator('button.vote').isEnabled(), true);
  assert.equal(await page.locator('button.vote').getAttribute('aria-pressed'), 'false');
  const sent = [];
  await page.route(`${origin}/api/boards/board1234/cards/card2/vote`, route => {
    const body = route.request().postDataJSON();
    sent.push(body);
    voting.revision++;
    for (const card of voting.columns[0].cards) { card.groupVoted = body.voted; card.groupVotes = body.voted ? 1 : 0; }
    return route.fulfill({ json: voting });
  });
  voting.revision = 2;
  await page.locator('button.vote').click();
  await page.waitForFunction(() => !document.querySelector('button.vote').disabled);
  assert.equal(await page.locator('button.vote').getAttribute('aria-pressed'), 'true');
  await page.locator('button.vote').click();
  await page.waitForFunction(() => !document.querySelector('button.vote').disabled);
  assert.equal(await page.locator('button.vote').getAttribute('aria-pressed'), 'false');
  assert.deepEqual(sent, [{ voted: true }, { voted: false }]);
});
