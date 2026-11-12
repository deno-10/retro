const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { execFile, spawn } = require('node:child_process');
const { once } = require('node:events');
const { randomBytes } = require('node:crypto');
const { mkdtemp, rm } = require('node:fs/promises');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const { setTimeout: sleep } = require('node:timers/promises');
const { promisify } = require('node:util');
const { chromium } = require('playwright');

const root = path.resolve(__dirname, '../..');
const token = () => randomBytes(16).toString('hex');
let directory, server, browser, origin;
let serverLog = '';

async function unusedPort() {
  const listener = net.createServer();
  listener.listen(0, '127.0.0.1');
  await once(listener, 'listening');
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  return port;
}

before(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), 'retro-integration-'));
  const binary = path.join(directory, process.platform === 'win32' ? 'retro.exe' : 'retro');
  await promisify(execFile)('go', ['build', '-o', binary, '.'], { cwd: root, timeout: 120000 });
  const port = await unusedPort();
  origin = `http://127.0.0.1:${port}`;
  server = spawn(binary, ['-addr', `127.0.0.1:${port}`, '-data', path.join(directory, 'retro.db')], {
    cwd: root, stdio: ['ignore', 'pipe', 'pipe'],
    env: Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('JIRA_') && key !== 'RETRO_PUBLIC_URL')),
  });
  server.stdout.on('data', chunk => { serverLog = (serverLog + chunk).slice(-16000); });
  server.stderr.on('data', chunk => { serverLog = (serverLog + chunk).slice(-16000); });
  server.on('error', error => { serverLog += error.message; });
  const deadline = Date.now() + 15000;
  for (;;) {
    if (server.exitCode !== null || server.signalCode !== null) throw new Error(`Сервер не запустился: ${serverLog}`);
    try {
      const response = await fetch(`${origin}/api/templates`, { signal: AbortSignal.timeout(500) });
      if (response.ok) break;
    } catch { /* ожидаем открытия порта */ }
    if (Date.now() > deadline) throw new Error(`Сервер не ответил: ${serverLog}`);
    await sleep(50);
  }
  browser = await chromium.launch({
    headless: true,
    // Playwright обычно отключает BFCache; здесь проверяем настоящий Back/Forward.
    ignoreDefaultArgs: ['--disable-back-forward-cache'],
    // В Headless Shell этот сценарий не попадает в BFCache. Полноценный
    // Chromium использует тот же жизненный цикл страницы, что обычный браузер.
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : { channel: 'chromium' }),
  });
}, { timeout: 180000 });

after(async () => {
  try {
    await browser?.close();
  } finally {
    try {
      if (server?.pid && server.exitCode === null && server.signalCode === null) {
        const exited = once(server, 'exit');
        server.kill('SIGTERM');
        await Promise.race([exited, sleep(10000, undefined, { ref: false })]);
        if (server.exitCode === null && server.signalCode === null) {
          server.kill('SIGKILL');
          await exited;
        }
      }
    } finally {
      if (directory) await rm(directory, { recursive: true, force: true });
    }
  }
});

async function api(method, route, participant, body, key) {
  const response = await fetch(origin + route, {
    method,
    headers: {
      'Content-Type': 'application/json', 'X-Participant': participant,
      ...(key ? { 'Idempotency-Key': key } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(5000),
  });
  return { status: response.status, data: await response.json() };
}

async function fixture() {
  const owner = token(), author = token();
  const created = await api('POST', '/api/boards', owner, { title: `Интеграция ${token()}`, template: 'classic' });
  assert.equal(created.status, 201);
  const id = created.data.id;
  assert.equal((await api('POST', `/api/boards/${id}/phase`, owner, { phase: 'reveal' })).status, 200);
  const added = await api('POST', `/api/boards/${id}/actions`, author,
    { text: 'Исходное решение', assignee: 'Анна' }, token());
  assert.equal(added.status, 200);
  return { owner, author, id, action: added.data.actions[0], board: added.data };
}

async function pageFor(t, fixture, participant, { disconnected = false } = {}) {
  const context = await browser.newContext();
  t.after(() => context.close());
  await context.addInitScript(participant => {
    localStorage.setItem('retro.participant', participant);
    localStorage.setItem('retro.name', 'Участник');
  }, participant);
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  if (disconnected) {
    await page.route(`${origin}/api/boards/${fixture.id}/events?*`, route => route.abort());
    await allowInitialBoardRead(page, fixture.id);
  }
  await page.goto(`${origin}/b/${fixture.id}`);
  await page.getByText('Исходное решение', { exact: true }).waitFor();
  return page;
}

async function allowInitialBoardRead(page, id) {
  // Только первое чтение разрешено: результат мутации должен попасть в UI
  // из её HTTP-ответа, а не случайно из фонового опроса.
  const url = `${origin}/api/boards/${id}`;
  await page.unroute(url);
  let reads = 0;
  await page.route(url, route => ++reads === 1 ? route.continue() : route.abort());
}

const field = (page, key) => page.locator(`[data-keep="${key}"]`);

async function holdCommittedResponse(t, page, url) {
  let release, committed, failed;
  const blocked = new Promise(resolve => { release = resolve; });
  const saved = new Promise((resolve, reject) => { committed = resolve; failed = reject; });
  t.after(release);
  await page.route(url, async route => {
    try {
      const response = await route.fetch();
      assert.equal(response.status(), 200);
      committed({ body: route.request().postDataJSON(), key: route.request().headers()['idempotency-key'] });
      await blocked; // Сервер записал данные, но браузер не получил даже заголовков.
      await route.abort().catch(() => {}); // Клиент мог уже отменить запрос по таймауту.
    } catch (error) {
      failed(error);
      await route.abort().catch(() => {});
    }
  }, { times: 1 });
  return { saved, release };
}

test('Два участника: устаревший PATCH отклоняется, черновик сохраняется и конфликт можно разрешить', { timeout: 30000 }, async t => {
  const f = await fixture();
  const page = await pageFor(t, f, f.author);
  await page.locator('.action').getByRole('button', { name: 'Изменить', exact: true }).click();
  await field(page, `action-edit:${f.action.id}`).fill('Мой несохранённый текст');
  const changed = await api('PATCH', `/api/boards/${f.id}/actions/${f.action.id}`, f.owner,
    { text: f.action.text, assignee: 'Борис', revision: f.action.revision });
  assert.equal(changed.status, 200);
  const failedSave = page.waitForResponse(r => r.request().method() === 'PATCH');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.equal((await failedSave).status(), 409);
  await page.getByRole('button', { name: 'Загрузить актуальную версию', exact: true }).waitFor();
  assert.equal(await field(page, `action-edit:${f.action.id}`).inputValue(), 'Мой несохранённый текст');
  let current = (await api('GET', `/api/boards/${f.id}`, f.owner)).data.actions[0];
  assert.equal(current.assignee, 'Борис');
  assert.equal(current.text, 'Исходное решение');
  await page.getByRole('button', { name: 'Загрузить актуальную версию', exact: true }).click();
  assert.equal(await field(page, `action-edit-assignee:${f.action.id}`).inputValue(), 'Борис');
  await field(page, `action-edit:${f.action.id}`).fill('Согласованный текст');
  const saved = page.waitForResponse(r => r.request().method() === 'PATCH');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.equal((await saved).status(), 200);
  await page.getByText('Согласованный текст', { exact: true }).waitFor();
  current = (await api('GET', `/api/boards/${f.id}`, f.owner)).data.actions[0];
  assert.equal(current.assignee, 'Борис');
  assert.equal(current.text, 'Согласованный текст');
});

test('При недоступных SSE и фоновом GET успешные POST/PATCH обновляют интерфейс', { timeout: 30000 }, async t => {
  const f = await fixture();
  const page = await pageFor(t, f, f.owner, { disconnected: true });
  const col = f.board.columns[0].id;
  await field(page, `draft:${col}`).fill('Карточка через HTTP');
  await page.locator('.column').filter({ has: field(page, `draft:${col}`) })
    .getByRole('button', { name: 'Добавить', exact: true }).click();
  await page.getByText('Карточка через HTTP', { exact: true }).waitFor();
  const card = page.locator('.card').filter({ has: page.getByText('Карточка через HTTP', { exact: true }) });
  await card.getByRole('button', { name: 'Изменить', exact: true }).click();
  const editor = page.locator('textarea[data-keep^="edit:"]');
  await editor.fill('Исправлено через HTTP');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  await page.getByText('Исправлено через HTTP', { exact: true }).waitFor();
  const current = (await api('GET', `/api/boards/${f.id}`, f.owner)).data;
  assert.equal(current.columns[0].cards[0].text, 'Исправлено через HTTP');
});

for (const kind of ['cards', 'actions']) {
  for (const reload of [false, true]) {
    test(`Потерянный ответ POST ${kind} и повтор ${reload ? 'после F5' : 'из интерфейса'} не создают дубликат`, { timeout: 30000 }, async t => {
      const f = await fixture();
      const page = await pageFor(t, f, f.owner, { disconnected: true });
      const url = `${origin}/api/boards/${f.id}/${kind}`;
      const keys = [];
      page.on('request', request => {
        if (request.url() === url && request.method() === 'POST') keys.push(request.headers()['idempotency-key']);
      });
      await page.route(url, async route => {
        const response = await route.fetch(); // Сервер уже сохранил запись.
        assert.equal(response.status(), 200);
        await route.abort('failed'); // Клиент не получил ответ.
      }, { times: 1 });
      const col = f.board.columns[0].id;
      const textKey = kind === 'cards' ? `draft:${col}` : 'action-text';
      await field(page, textKey).fill('Запись с потерянным ответом');
      await page.locator('.column').filter({ has: field(page, textKey) })
        .getByRole('button', { name: kind === 'cards' ? 'Добавить' : 'Записать', exact: true }).click();
      const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
      await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).waitFor();
      if (reload) {
        await allowInitialBoardRead(page, f.id);
        await page.reload();
        await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).waitFor();
        assert.equal(keys.length, 1, 'После перезагрузки повтор должен ждать явного действия');
      }
      await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
      await recovery.getByText('Запись с потерянным ответом', { exact: true }).waitFor({ state: 'detached' });
      await page.getByText('Запись с потерянным ответом', { exact: true }).waitFor();
      assert.equal(keys.length, 2);
      assert.match(keys[0], /^[a-f0-9]{32}$/);
      assert.equal(keys[1], keys[0]);
      const current = (await api('GET', `/api/boards/${f.id}`, f.owner)).data;
      const items = kind === 'cards' ? current.columns[0].cards : current.actions;
      assert.equal(items.filter(item => item.text === 'Запись с потерянным ответом').length, 1);
    });
  }
}

test('Ошибка отметки откатывает галочку; повторное done=true не отменяет выполнение', { timeout: 30000 }, async t => {
  const f = await fixture();
  const page = await pageFor(t, f, f.author);
  const url = `${origin}/api/boards/${f.id}/actions/${f.action.id}/done`;
  await page.route(url, route => route.fulfill({ status: 503, json: { error: 'Ошибка записи отметки' } }), { times: 1 });
  const checkbox = page.locator('.action input[type="checkbox"]');
  // Ошибка намеренно вернёт галочку в false; check() может повторить клик,
  // пытаясь дождаться true. Проверяем один пользовательский клик.
  let requests = 0;
  page.on('request', request => { if (request.url() === url) requests++; });
  await checkbox.click();
  await page.getByText('Ошибка записи отметки', { exact: true }).waitFor();
  // Ждём завершения обработки ошибки, включая восстановление состояния.
  await page.waitForFunction(() => {
    const el = document.querySelector('.action input[type="checkbox"]');
    return el && !el.checked && !el.disabled;
  });
  assert.equal(requests, 1);
  assert.equal((await api('GET', `/api/boards/${f.id}`, f.owner)).data.actions[0].done, false);
  const saved = page.waitForResponse(r => r.url() === url && r.status() === 200);
  await checkbox.check();
  await saved;
  await page.waitForFunction(() => {
    const el = document.querySelector('.action input[type="checkbox"]');
    return el && el.checked && !el.disabled;
  });
  for (const participant of [f.owner, f.author]) {
    const repeated = await api('POST', `/api/boards/${f.id}/actions/${f.action.id}/done`, participant, { done: true });
    assert.equal(repeated.status, 200);
    assert.equal(repeated.data.actions[0].done, true);
  }
});

test('F5 сохраняет оба редактора, черновики новых записей и ответственных', { timeout: 30000 }, async t => {
  const f = await fixture();
  const col = f.board.columns[0].id;
  const added = await api('POST', `/api/boards/${f.id}/cards`, f.author,
    { columnId: col, text: 'Исходная карточка', author: 'Анна' }, token());
  assert.equal(added.status, 200);
  const cardId = added.data.columns[0].cards[0].id;
  const page = await pageFor(t, f, f.author);
  await page.locator('.card.mine').getByRole('button', { name: 'Изменить', exact: true }).click();
  await page.locator('.action').getByRole('button', { name: 'Изменить', exact: true }).click();
  const values = {
    [`draft:${col}`]: 'Черновик новой карточки',
    'action-text': 'Черновик нового решения',
    'action-assignee': 'Борис',
    [`edit:${cardId}`]: 'Несохранённая правка карточки',
    [`action-edit:${f.action.id}`]: 'Несохранённая правка решения',
    [`action-edit-assignee:${f.action.id}`]: 'Вера',
  };
  for (const [key, value] of Object.entries(values)) await field(page, key).fill(value);
  await page.reload();
  for (const [key, value] of Object.entries(values)) {
    await field(page, key).waitFor();
    assert.equal(await field(page, key).inputValue(), value, key);
  }
  const current = (await api('GET', `/api/boards/${f.id}`, f.author)).data;
  assert.equal(current.columns[0].cards[0].text, 'Исходная карточка');
  assert.equal(current.actions[0].assignee, 'Анна');
});

test('Потеря ответа голосования и повтор клика сохраняют один голос', { timeout: 30000 }, async t => {
  const f = await fixture();
  const added = await api('POST', `/api/boards/${f.id}/cards`, f.owner,
    { columnId: f.board.columns[0].id, text: 'Кандидат для голосования' }, token());
  assert.equal(added.status, 200);
  const cardId = added.data.columns[0].cards[0].id;
  assert.equal((await api('POST', `/api/boards/${f.id}/phase`, f.owner, { phase: 'vote' })).status, 200);
  const page = await pageFor(t, f, f.owner, { disconnected: true });
  const url = `${origin}/api/boards/${f.id}/cards/${cardId}/vote`;
  const bodies = [];
  page.on('request', request => { if (request.url() === url) bodies.push(request.postDataJSON()); });
  await page.route(url, async route => {
    assert.equal((await route.fetch()).status(), 200);
    await route.abort('failed');
  }, { times: 1 });
  await page.getByTitle('Отдать голос', { exact: true }).click();
  await page.getByText('Не удалось получить ответ сервера. Проверьте соединение.', { exact: true }).waitFor();
  assert.equal((await api('GET', `/api/boards/${f.id}`, f.owner)).data.columns[0].cards[0].votes, 1);
  const repeated = page.waitForResponse(response => response.url() === url && response.status() === 200);
  await page.getByTitle('Отдать голос', { exact: true }).click();
  await repeated;
  await page.getByTitle('Снять голос', { exact: true }).waitFor();
  assert.deepEqual(bodies, [{ voted: true }, { voted: true }]);
  assert.equal((await api('GET', `/api/boards/${f.id}`, f.owner)).data.columns[0].cards[0].votes, 1);
  await page.getByTitle('Снять голос', { exact: true }).click();
  await page.getByTitle('Отдать голос', { exact: true }).waitFor();
  const removedAgain = await api('POST', `/api/boards/${f.id}/cards/${cardId}/vote`, f.owner, { voted: false });
  assert.equal(removedAgain.status, 200);
  assert.equal(removedAgain.data.columns[0].cards[0].votes, 0);
});

test('Таймаут POST после записи на сервере сохраняет ключ и позволяет повтор без дубля', { timeout: 30000 }, async t => {
  const f = await fixture();
  const page = await pageFor(t, f, f.owner, { disconnected: true });
  await page.clock.install();
  const url = `${origin}/api/boards/${f.id}/cards`;
  const held = await holdCommittedResponse(t, page, url);
  const keys = [];
  page.on('request', request => { if (request.url() === url) keys.push(request.headers()['idempotency-key']); });
  await field(page, `draft:${f.board.columns[0].id}`).fill('Ответ задержался после сохранения');
  await page.getByRole('button', { name: 'Добавить', exact: true }).first().click();
  const original = await held.saved;
  await page.clock.fastForward(16000);
  const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
  const retry = recovery.getByRole('button', { name: 'Повторить отправку', exact: true });
  await retry.waitFor();
  await page.waitForFunction(() => !document.querySelector('[data-send^="card:"]').disabled);
  await retry.click();
  await recovery.getByText('Ответ задержался после сохранения', { exact: true }).waitFor({ state: 'detached' });
  assert.deepEqual(keys, [original.key, original.key]);
  const current = (await api('GET', `/api/boards/${f.id}`, f.owner)).data;
  assert.equal(current.columns[0].cards.length, 1);
  held.release();
});

test('Таймаут PATCH освобождает редактор и сверяет уже сохранённую правку с сервером', { timeout: 30000 }, async t => {
  const f = await fixture();
  const page = await pageFor(t, f, f.author);
  await page.clock.install();
  const url = `${origin}/api/boards/${f.id}/actions/${f.action.id}`;
  const held = await holdCommittedResponse(t, page, url);
  await page.locator('.action').getByRole('button', { name: 'Изменить', exact: true }).click();
  await field(page, `action-edit:${f.action.id}`).fill('Ответ PATCH не пришёл');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  await held.saved;
  await page.clock.fastForward(16000);
  await page.locator('.edit-conflict').getByText('Ответ PATCH не пришёл', { exact: true }).waitFor();
  assert.equal(await field(page, `action-edit:${f.action.id}`).isDisabled(), false);
  assert.equal(await field(page, `action-edit:${f.action.id}`).inputValue(), 'Ответ PATCH не пришёл');
  assert.equal(await page.getByRole('button', { name: 'Сохранить', exact: true }).isDisabled(), true);
  await page.getByRole('button', { name: 'Отмена', exact: true }).click();
  await field(page, `action-edit:${f.action.id}`).waitFor({ state: 'detached' });
  await page.locator('.action').getByText('Ответ PATCH не пришёл', { exact: true }).waitFor();
  const current = (await api('GET', `/api/boards/${f.id}`, f.author)).data;
  assert.equal(current.actions[0].text, 'Ответ PATCH не пришёл');
  held.release();
});

async function traverseHistory(page, direction, pathname) {
  // BFCache не вызывает новый load. В некоторых Chromium goBack/goForward
  // уже меняют документ, но продолжают ожидать навигационное событие.
  try {
    await page[direction]({ waitUntil: 'commit', timeout: 1500 });
  } catch (error) {
    if (error.name !== 'TimeoutError') throw error;
  }
  await page.waitForFunction(pathname => location.pathname === pathname, pathname);
}

test('Настоящий Back/Forward с SSE сохраняет новую очередь и не восстанавливает уже отправленную запись', { timeout: 30000 }, async t => {
  const f = await fixture();
  const context = await browser.newContext();
  t.after(() => context.close());
  await context.addInitScript(owner => {
    localStorage.setItem('retro.participant', owner);
    window.testDocumentID = crypto.randomUUID();
    window.testRestored = false;
    addEventListener('pageshow', event => { window.testRestored = event.persisted; });
    const nativeFetch = window.fetch;
    window.fetch = (url, options) => {
      if (window.testFailNextCreate && options?.method === 'POST' && String(url).endsWith('/cards')) {
        window.testFailNextCreate = false;
        return Promise.resolve(new Response(JSON.stringify({ error: 'Пробная ошибка отправки' }), {
          status: 503, headers: { 'Content-Type': 'application/json' },
        }));
      }
      return nativeFetch(url, options);
    };
  }, f.owner);
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  const path = `/b/${f.id}`;
  const storageKey = `retro.drafts.v1:${f.id}:${f.owner}`;
  const snapshot = () => page.evaluate(key => JSON.parse(sessionStorage.getItem(key)), storageKey);
  const editor = field(page, `draft:${f.board.columns[0].id}`);
  await page.goto(origin + path);
  await editor.fill('Первый черновик A');
  const firstDocument = await page.evaluate(() => window.testDocumentID);
  await page.goto(origin + '/');
  await page.getByRole('link', { name: f.board.title, exact: true }).click();
  await editor.waitFor();
  const secondDocument = await page.evaluate(() => window.testDocumentID);
  assert.notEqual(secondDocument, firstDocument);
  await page.evaluate(() => { window.testFailNextCreate = true; });
  await editor.fill('Карточка из новой очереди');
  await page.locator('.column').filter({ has: editor }).getByRole('button', { name: 'Добавить', exact: true }).click();
  const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
  await recovery.getByText('Пробная ошибка отправки', { exact: true }).waitFor();
  await editor.fill('Последний черновик C');
  const saved = await snapshot();
  assert.equal(saved.outbox.length, 1);

  await traverseHistory(page, 'goBack', '/');
  await traverseHistory(page, 'goBack', path);
  await page.waitForFunction(id => window.testRestored && window.testDocumentID === id, firstDocument);
  assert.equal(await editor.inputValue(), 'Последний черновик C');
  await recovery.getByText('Карточка из новой очереди', { exact: true }).waitFor();
  await editor.press('End');
  await editor.pressSequentially(' +');
  assert.deepEqual((await snapshot()).outbox, saved.outbox, 'Старый документ не должен стереть новую очередь');
  const retry = page.waitForRequest(request => request.url().endsWith(`/api/boards/${f.id}/cards`));
  await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
  assert.equal((await retry).headers()['idempotency-key'], saved.outbox[0].options.idempotencyKey);
  await recovery.getByText('Карточка из новой очереди', { exact: true }).waitFor({ state: 'detached' });
  assert.equal((await snapshot()).outbox.length, 0);

  await traverseHistory(page, 'goForward', '/');
  await traverseHistory(page, 'goForward', path);
  await page.waitForFunction(id => window.testRestored && window.testDocumentID === id, secondDocument);
  assert.equal(await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).count(), 0);
  await editor.fill('Новый ввод после Forward');
  assert.equal((await snapshot()).outbox.length, 0, 'Отправленная запись не должна воскреснуть в старом документе');
  const current = (await api('GET', `/api/boards/${f.id}`, f.owner)).data;
  assert.equal(current.columns[0].cards.filter(card => card.text === 'Карточка из новой очереди').length, 1);
});

test('Отказ localStorage не меняет владельца, черновик и ключ повторной отправки после F5', { timeout: 30000 }, async t => {
  const context = await browser.newContext();
  t.after(() => context.close());
  await context.addInitScript(() => {
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function (...args) {
      if (this === localStorage) throw new DOMException('Квота localStorage', 'QuotaExceededError');
      return setItem.apply(this, args);
    };
  });
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  await page.goto(origin + '/');
  await page.getByLabel('Название', { exact: true }).fill('Владелец с резервным ID');
  const creating = page.waitForRequest(request => request.method() === 'POST' && request.url() === `${origin}/api/boards`);
  await page.getByRole('button', { name: 'Создать доску', exact: true }).click();
  const owner = (await creating).headers()['x-participant'];
  assert.match(owner, /^[a-f0-9]{32}$/);
  await page.waitForURL(/\/b\/[a-f0-9]+$/);
  const id = new URL(page.url()).pathname.split('/').pop();
  const board = (await api('GET', `/api/boards/${id}`, owner)).data;
  assert.equal(board.isOwner, true);
  const editor = field(page, `draft:${board.columns[0].id}`);
  const url = `${origin}/api/boards/${id}/cards`;
  let originalKey;
  await page.route(url, async route => {
    originalKey = route.request().headers()['idempotency-key'];
    assert.equal((await route.fetch()).status(), 200);
    await route.abort('failed');
  }, { times: 1 });
  await editor.fill('Создано до потери ответа');
  await page.locator('.column').filter({ has: editor }).getByRole('button', { name: 'Добавить', exact: true }).click();
  const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
  await recovery.getByText('Не удалось получить ответ сервера. Проверьте соединение.', { exact: true }).waitFor();
  await editor.fill('Черновик до обновления');
  const reloading = page.waitForRequest(request => request.method() === 'GET' && request.url() === `${origin}/api/boards/${id}`);
  await page.reload();
  assert.equal((await reloading).headers()['x-participant'], owner);
  await editor.waitFor();
  assert.equal(await editor.inputValue(), 'Черновик до обновления');
  await page.getByTitle('Перейти: Показ', { exact: true }).waitFor();
  const retried = page.waitForRequest(request => request.url() === url);
  await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
  const request = await retried;
  assert.equal(request.headers()['x-participant'], owner);
  assert.equal(request.headers()['idempotency-key'], originalKey);
  await recovery.getByText('Создано до потери ответа', { exact: true }).waitFor({ state: 'detached' });
  const current = (await api('GET', `/api/boards/${id}`, owner)).data;
  assert.equal(current.columns[0].cards.length, 1);
});

test('Пустой таймер и лишние данные после JSON отклоняются до изменения доски', { timeout: 30000 }, async () => {
  const f = await fixture();
  const route = `/api/boards/${f.id}/timer`;
  const started = await api('POST', route, f.owner, { minutes: 2 });
  assert.equal(started.status, 200);
  for (const body of ['{}', '{"minutes":null}', 'null', '{"minutes":0} THIS IS NOT JSON', '{"minutes":0} null']) {
    const response = await fetch(origin + route, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Participant': f.owner },
      body, signal: AbortSignal.timeout(5000),
    });
    assert.equal(response.status, 400, body);
    await response.arrayBuffer();
    const current = (await api('GET', `/api/boards/${f.id}`, f.owner)).data;
    assert.equal(current.timerEndsAt, started.data.timerEndsAt);
    assert.equal(current.revision, started.data.revision);
  }
  const stopped = await fetch(origin + route, {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Participant': f.owner },
    body: '{"minutes":0} \n\t', signal: AbortSignal.timeout(5000),
  });
  assert.equal(stopped.status, 200);
  assert.equal((await stopped.json()).timerEndsAt, undefined);
});
