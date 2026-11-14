const { test: nodeTest, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { asset } = require('./assets.cjs');
const { chromium } = require('playwright');
const test = (name, fn) => nodeTest(name, { timeout: 15000 }, fn);
const origin = 'http://retro.test';
const copy = value => structuredClone(value);
let browser;
before(async () => {
  browser = await chromium.launch({ headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}),
  });
});
after(async () => { await browser?.close(); });

async function openBoard(t, clock = false) {
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  t.after(() => page.close());
  if (clock) await page.clock.install();
  let serverBoard = {
    id: 'board1234', title: 'Доска', revision: 1, phase: 'reveal', isOwner: true,
    voteLimit: 3, votesLeft: 3, online: 1,
    columns: [{ id: 'col1', title: 'Хорошо', cards: [
      { id: 'card1', text: 'Исходная карточка', author: 'Анна', mine: true, revision: 1 },
    ] }],
    actions: [{ id: 'action1', text: 'Исходное решение', assignee: 'Анна', mine: true, done: false, revision: 1 }],
  };
  let reads = 0;
  await page.addInitScript(() => {

    window.EventSource = class {
      constructor() { window.testEvents = this; this.listeners = {}; this.closed = false; }
      addEventListener(name, listener) { this.listeners[name] = listener; }
      close() { this.closed = true; }
    };
  });
  await page.route(`${origin}/**`, route => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname === '/api/boards/board1234') { reads++; return route.fulfill({ json: serverBoard }); }
    if (pathname === '/b/board1234') return route.fulfill({
      contentType: 'text/html', body: asset('index.html'),
    });
    const name = pathname === '/static/app.js' ? 'app.js' : pathname === '/static/style.css' ? 'style.css' : null;
    if (name) return route.fulfill({
      contentType: name.endsWith('.js') ? 'application/javascript' : 'text/css',
      body: asset(name),
    });
    return route.fulfill({ status: 404, json: { error: 'Неожиданный запрос' } });
  });
  await page.goto(`${origin}/b/board1234`);
  await page.getByText('Исходное решение', { exact: true }).waitFor();
  return {
    page,
    get board() { return copy(serverBoard); },
    set board(value) { serverBoard = copy(value); },
    get reads() { return reads; },
    emit(value) { return page.evaluate(data => window.testEvents.listeners.board({ data: JSON.stringify({ ...data, participant: document.querySelector('meta[name="retro-participant"]').content }) }), value); },
    disconnect() { return page.evaluate(() => window.testEvents.onerror()); },
  };
}
const field = (page, key) => page.locator(`[data-keep="${key}"]`);
async function edit(page, kind) {
  await page.locator(kind === 'action' ? '.action' : '.card.mine')
    .getByRole('button', { name: 'Изменить', exact: true }).click();
}
async function hold(page, suffix) {
  let start, release;
  const request = new Promise(resolve => { start = resolve; });
  const response = new Promise(resolve => { release = resolve; });
  await page.route(`${origin}/api/boards/board1234${suffix}`, async route => {
    start({ body: route.request().postDataJSON(), headers: route.request().headers() });
    const result = await response;
    if (result.abort) await route.abort();
    else await route.fulfill(result);
  }, { times: 1 });
  return { request, respond: release };
}

// Ответы HTTP должны обновлять интерфейс и при недоступном потоке событий.
test('Успешные POST и PATCH обновляют доску без SSE', async t => {
  const env = await openBoard(t);
  const { page } = env;
  await env.disconnect();
  let next = env.board;
  next.revision = 2;
  next.columns[0].cards.push({ id: 'card2', text: 'Новая карточка', mine: true, revision: 1 });
  const post = await hold(page, '/cards');
  await field(page, 'draft:col1').fill('Новая карточка');
  await page.getByRole('button', { name: 'Добавить', exact: true }).click();
  await post.request;
  post.respond({ json: next });
  await page.getByText('Новая карточка', { exact: true }).waitFor();
  await page.locator('.card').filter({ hasText: 'Исходная карточка' })
    .getByRole('button', { name: 'Изменить', exact: true }).click();
  await field(page, 'edit:card1').fill('Сохранено через HTTP');
  const patch = await hold(page, '/cards/card1');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.deepEqual((await patch.request).body, { text: 'Сохранено через HTTP', revision: 1 });
  next.revision = 3;
  next.columns[0].cards[0] = { ...next.columns[0].cards[0], text: 'Сохранено через HTTP', revision: 2 };
  patch.respond({ json: next });
  await field(page, 'edit:card1').waitFor({ state: 'detached' });
  await page.getByText('Сохранено через HTTP', { exact: true }).waitFor();
});

test('Поздний HTTP не откатывает новую SSE-версию после PATCH', async t => {
  const env = await openBoard(t);
  const { page } = env;
  await edit(page, 'card');
  await field(page, 'edit:card1').fill('Моя правка');
  const patch = await hold(page, '/cards/card1');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  await patch.request;
  const older = env.board;
  older.revision = 2;
  older.columns[0].cards[0] = { ...older.columns[0].cards[0], text: 'Моя правка', revision: 2 };
  const newer = copy(older);
  newer.revision = 3;
  newer.columns[0].cards[0] = { ...newer.columns[0].cards[0], text: 'Более новая правка', revision: 3 };
  await env.emit(newer);
  patch.respond({ json: older });
  await field(page, 'edit:card1').waitFor({ state: 'detached' });
  await page.getByText('Более новая правка', { exact: true }).waitFor();
  assert.equal(await page.getByText('Моя правка', { exact: true }).count(), 0);
});

test('Поздний SSE не откатывает HTTP, одинаковая версия обновляет онлайн', async t => {
  const env = await openBoard(t);
  const { page } = env;
  const newer = env.board;
  newer.revision = 3;
  newer.title = 'Новая версия из HTTP';
  const response = await hold(page, '/timer');
  await page.getByRole('button', { name: 'Запустить', exact: true }).click();
  await response.request;
  response.respond({ json: newer });
  await page.getByRole('heading', { name: newer.title, exact: true }).waitFor();
  const older = env.board;
  older.revision = 2;
  older.title = 'Запоздалый SSE';
  await env.emit(older);
  assert.equal(await page.locator('.board-title').textContent(), newer.title);
  await env.emit({ ...newer, online: 9 });
  await page.getByText('Онлайн: 9', { exact: true }).waitFor();
});

test('Fallback GET выполняется по одному, переживает onopen и завершается событием board', async t => {
  const env = await openBoard(t, true);
  const { page } = env;
  const refresh = env.board;
  refresh.revision = 2;
  refresh.title = 'Обновлено через GET';
  const held = await hold(page, '');
  await env.disconnect();
  await held.request;
  await env.disconnect();
  await page.clock.fastForward(6000);
  assert.equal(env.reads, 1, 'Параллельный fallback GET не должен стартовать');
  held.respond({ json: refresh });
  await page.getByRole('heading', { name: refresh.title, exact: true }).waitFor();
  env.board = refresh;
  await page.evaluate(() => window.testEvents.onopen?.());
  const nextRead = page.waitForResponse(response => response.request().method() === 'GET' && response.url().endsWith('/api/boards/board1234'));
  await page.clock.fastForward(3000);
  await nextRead;
  assert.equal(env.reads, 2, 'onopen без board не должен останавливать fallback');
  await env.emit({ ...refresh, online: 2 });
  await page.clock.fastForward(6000);
  assert.equal(env.reads, 2, 'После board fallback должен остановиться');
  env.board = { ...refresh, revision: 3, title: 'Второе отключение' };
  const resumed = page.waitForResponse(response => response.request().method() === 'GET' && response.url().endsWith('/api/boards/board1234'));
  await env.disconnect();
  await resumed;
  await page.getByRole('heading', { name: 'Второе отключение', exact: true }).waitFor();
  assert.equal(env.reads, 3, 'Следующий разрыв должен снова включить fallback');
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide')));
  assert.equal(await page.evaluate(() => window.testEvents.closed), true);
  await env.disconnect();
  await page.clock.fastForward(6000);
  assert.equal(env.reads, 3, 'После pagehide нельзя запускать polling');
  const restored = page.waitForResponse(response => response.request().method() === 'GET' && response.url().endsWith('/api/boards/board1234'));
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })));
  await restored;
  assert.equal(await page.evaluate(() => window.testEvents.closed), false);
  assert.equal(env.reads, 4, 'Возврат из кеша страницы должен восстановить подписку и обновить доску');
});

test('Поздний fallback GET не откатывает более новую SSE-версию', async t => {
  const env = await openBoard(t);
  const { page } = env;
  const held = await hold(page, '');
  await env.disconnect();
  await held.request;
  const newer = env.board;
  newer.revision = 3;
  newer.title = 'Новый SSE';
  await env.emit(newer);
  const older = env.board;
  older.revision = 2;
  older.title = 'Старый GET';
  // Сервер мог уже отправить ответ до отмены запроса клиентом.
  held.respond({ json: older });
  await page.getByRole('heading', { name: 'Новый SSE', exact: true }).waitFor();
  assert.equal(await page.getByRole('heading', { name: 'Старый GET', exact: true }).count(), 0);
});

test('Конфликт решения сохраняет черновик и требует явного сохранения поверх актуальной версии', async t => {
  const env = await openBoard(t);
  const { page } = env;
  await edit(page, 'action');
  await field(page, 'action-edit:action1').fill('Мой текст');
  await field(page, 'action-edit-assignee:action1').fill('Мой ответственный');
  const remote = env.board;
  remote.revision = 2;
  remote.actions[0] = { ...remote.actions[0], text: 'Чужой текст', assignee: 'Борис', revision: 2 };
  env.board = remote;
  await env.emit(remote);
  const stale = await hold(page, '/actions/action1');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.deepEqual((await stale.request).body, { text: 'Мой текст', assignee: 'Мой ответственный', revision: 1 });
  stale.respond({ status: 409, json: { error: 'Решение уже изменено' } });
  await page.getByRole('button', { name: 'Сохранить мой вариант поверх', exact: true }).waitFor();
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Мой текст');
  assert.equal(await field(page, 'action-edit-assignee:action1').inputValue(), 'Мой ответственный');
  await page.locator('.edit-conflict').getByText('Чужой текст', { exact: true }).waitFor();
  await page.locator('.edit-conflict').getByText('Ответственный: Борис', { exact: true }).waitFor();
  assert.equal(await page.getByRole('button', { name: 'Сохранить', exact: true }).isDisabled(), true);
  const overwrite = await hold(page, '/actions/action1');
  await page.getByRole('button', { name: 'Сохранить мой вариант поверх', exact: true }).click();
  assert.deepEqual((await overwrite.request).body, { text: 'Мой текст', assignee: 'Мой ответственный', revision: 2 });
  const saved = copy(remote);
  saved.revision = 3;
  saved.actions[0] = { ...saved.actions[0], text: 'Мой текст', assignee: 'Мой ответственный', revision: 3 };
  overwrite.respond({ json: saved });
  await field(page, 'action-edit:action1').waitFor({ state: 'detached' });
  await page.locator('.action').getByText('Мой текст', { exact: true }).waitFor();
});

test('Конфликт карточки позволяет явно загрузить актуальную версию и редактировать её', async t => {
  const env = await openBoard(t);
  const { page } = env;
  await edit(page, 'card');
  await field(page, 'edit:card1').fill('Мой черновик');
  const remote = env.board;
  remote.revision = 2;
  remote.columns[0].cards[0] = { ...remote.columns[0].cards[0], text: 'Актуальная карточка', revision: 2 };
  env.board = remote;
  const stale = await hold(page, '/cards/card1');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.equal((await stale.request).body.revision, 1);
  stale.respond({ status: 409, json: { error: 'Карточка уже изменена' } });
  await page.getByRole('button', { name: 'Загрузить актуальную версию', exact: true }).click();
  assert.equal(await field(page, 'edit:card1').inputValue(), 'Актуальная карточка');
  await field(page, 'edit:card1').fill('Уточнение актуальной карточки');
  const update = await hold(page, '/cards/card1');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.deepEqual((await update.request).body, { text: 'Уточнение актуальной карточки', revision: 2 });
  const saved = copy(remote);
  saved.revision = 3;
  saved.columns[0].cards[0] = { ...saved.columns[0].cards[0], text: 'Уточнение актуальной карточки', revision: 3 };
  update.respond({ json: saved });
  await field(page, 'edit:card1').waitFor({ state: 'detached' });
  await page.getByText('Уточнение актуальной карточки', { exact: true }).waitFor();
});

test('Отметка решения отправляет желаемое значение, блокируется при SSE и откатывается при ошибке', async t => {
  const env = await openBoard(t);
  const { page } = env;
  const checkbox = page.locator('.action input[type="checkbox"]');
  const failed = await hold(page, '/actions/action1/done');
  await checkbox.check();
  assert.deepEqual((await failed.request).body, { done: true });
  await env.emit({ ...env.board, revision: 2 });
  assert.equal(await checkbox.isChecked(), true);
  assert.equal(await checkbox.isDisabled(), true);
  failed.respond({ status: 503, json: { error: 'Не удалось отметить решение' } });
  await page.getByText('Не удалось отметить решение', { exact: true }).waitFor();
  assert.equal(await checkbox.isChecked(), false);
  assert.equal(await checkbox.isDisabled(), false);
  const success = await hold(page, '/actions/action1/done');
  await checkbox.check();
  assert.deepEqual((await success.request).body, { done: true });
  const done = env.board;
  done.revision = 3;
  done.actions[0].done = true;
  success.respond({ json: done });
  await page.waitForFunction(() => !document.querySelector('.action input[type="checkbox"]').disabled);
  assert.equal(await checkbox.isChecked(), true);
  const clear = await hold(page, '/actions/action1/done');
  await checkbox.uncheck();
  assert.deepEqual((await clear.request).body, { done: false });
  done.revision = 4;
  done.actions[0].done = false;
  clear.respond({ json: done });
  await page.waitForFunction(() => !document.querySelector('.action input[type="checkbox"]').disabled);
  assert.equal(await checkbox.isChecked(), false);
});

for (const kind of ['cards', 'actions']) {
  test(`Повтор ${kind} после потери ответа использует прежний Idempotency-Key, новая запись — новый`, async t => {
    const env = await openBoard(t);
    const { page } = env;
    const key = kind === 'cards' ? 'draft:col1' : 'action-text';
    const button = kind === 'cards' ? 'Добавить' : 'Записать';
    const first = await hold(page, `/${kind}`);
    await field(page, key).fill('Запись с потерянным ответом');
    await page.getByRole('button', { name: button, exact: true }).click();
    const original = await first.request;
    assert.match(original.headers['idempotency-key'], /^[a-f0-9]{32}$/);
    first.respond({ abort: true });
    const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
    await recovery.getByText('Запись с потерянным ответом', { exact: true }).waitFor();
    await field(page, key).fill('Новая операция');
    const retry = await hold(page, `/${kind}`);
    await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
    const repeated = await retry.request;
    assert.equal(repeated.headers['idempotency-key'], original.headers['idempotency-key']);
    assert.deepEqual(repeated.body, original.body);
    retry.respond({ json: { ...env.board, revision: 2 } });
    await recovery.getByText('Запись с потерянным ответом', { exact: true }).waitFor({ state: 'detached' });
    assert.equal(await field(page, key).inputValue(), 'Новая операция');
    const next = await hold(page, `/${kind}`);
    await page.getByRole('button', { name: button, exact: true }).click();
    const independent = await next.request;
    assert.match(independent.headers['idempotency-key'], /^[a-f0-9]{32}$/);
    assert.notEqual(independent.headers['idempotency-key'], original.headers['idempotency-key']);
    next.respond({ json: { ...env.board, revision: 3 } });
  });
}

for (const kind of ['card', 'action']) {
  const editKey = kind === 'card' ? 'edit:card1' : 'action-edit:action1';
  const suffix = kind === 'card' ? '/cards/card1' : '/actions/action1';
  const withoutItem = board => {
    const result = copy(board);
    result.revision = 3;
    if (kind === 'card') result.columns[0].cards = [];
    else result.actions = [];
    return result;
  };

  test(`Конфликт ${kind} с удалённой записью оставляет доступный черновик до явного закрытия`, async t => {
    const env = await openBoard(t);
    const { page } = env;
    await edit(page, kind);
    await field(page, editKey).fill('Правки удалённой записи');
    if (kind === 'action') await field(page, 'action-edit-assignee:action1').fill('Новый ответственный');
    const stale = await hold(page, suffix);
    await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
    await stale.request;
    env.board = withoutItem(env.board);
    stale.respond({ status: 409, json: { error: 'Запись уже изменена' } });
    const orphan = page.getByRole('region', { name: 'Правки удалённых записей' });
    await orphan.waitFor();
    await page.waitForFunction(key => !document.querySelector(`[data-keep="${key}"]`).disabled, editKey);
    assert.equal(await field(orphan, editKey).inputValue(), 'Правки удалённой записи');
    if (kind === 'action') assert.equal(await field(orphan, 'action-edit-assignee:action1').inputValue(), 'Новый ответственный');
    await orphan.getByText('Запись уже удалена. Скопируйте нужный текст из редактора.', { exact: true }).waitFor();
    await env.emit({ ...env.board, revision: 4 });
    assert.equal(await field(orphan, editKey).inputValue(), 'Правки удалённой записи');
    await orphan.getByRole('button', { name: 'Закрыть черновик', exact: true }).click();
    await orphan.waitFor({ state: 'detached' });
  });

  test(`Поздний успешный PATCH ${kind} после SSE удаления не уничтожает черновик`, async t => {
    const env = await openBoard(t);
    const { page } = env;
    await edit(page, kind);
    await field(page, editKey).fill('Текст сохранённого затем удалённого объекта');
    const pending = await hold(page, suffix);
    await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
    await pending.request;
    const olderSuccess = env.board;
    olderSuccess.revision = 2;
    const savedItem = kind === 'card' ? olderSuccess.columns[0].cards[0] : olderSuccess.actions[0];
    savedItem.text = 'Текст сохранённого затем удалённого объекта';
    savedItem.revision = 2;
    await env.emit(withoutItem(olderSuccess));
    const orphan = page.getByRole('region', { name: 'Правки удалённых записей' });
    await orphan.waitFor();
    assert.equal(await field(orphan, editKey).isDisabled(), true);
    pending.respond({ json: olderSuccess });
    await page.waitForFunction(key => !document.querySelector(`[data-keep="${key}"]`).disabled, editKey);
    assert.equal(await field(orphan, editKey).inputValue(), 'Текст сохранённого затем удалённого объекта');
    assert.equal(await orphan.getByRole('button', { name: 'Сохранить', exact: true }).isDisabled(), true);
    assert.equal(await orphan.getByRole('button', { name: 'Закрыть черновик', exact: true }).isEnabled(), true);
  });
}
