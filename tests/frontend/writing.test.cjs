const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { asset } = require('./assets.cjs');
const { chromium } = require('playwright');
const origin = 'http://retro.test';
let browser;
before(async () => {
  browser = await chromium.launch({ headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}),
  });
});
after(async () => browser?.close());

const finish = page => page.getByRole('button', { name: 'Я закончил', exact: true });
const resume = page => page.getByRole('button', { name: 'Продолжить писать', exact: true });

async function fixture(t, changes = {}) {
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  t.after(() => page.close());
  const board = { id: 'writing1234', title: 'Готовность команды', phase: 'write', revision: 1, isOwner: true,
    voteLimit: 3, votesLeft: 3, writing: { round: 1, participants: 2, ready: 0, mine: false, joined: true, revision: 0 },
    columns: [{ id: 'col1', title: 'Хорошо', cards: [{ id: 'card1', text: 'Сохранённая карточка', mine: true, revision: 1 }] }],
    actions: [], ...changes,
  };
  const sent = [];
  const env = { page, board, sent, readyHandler: null, joinHandler: null, cardHandler: null };
  const applyReady = body => {
    board.writing.mine = body.ready;
    board.writing.ready += body.ready ? 1 : -1;
    board.writing.revision++;
    board.revision++;
  };
  env.applyReady = applyReady;
  await page.addInitScript(() => {

    window.EventSource = class {
      constructor() { window.testEvents = this; this.listeners = {}; }
      addEventListener(name, callback) { this.listeners[name] = callback; }
      close() {}
    };
  });
  await page.route(`${origin}/**`, async route => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname === '/api/boards/writing1234') return route.fulfill({ json: board });
    if (pathname.endsWith('/writing/ready')) {
      const body = route.request().postDataJSON(); sent.push(body);
      if (env.readyHandler) return env.readyHandler(route, body);
      applyReady(body);
      return route.fulfill({ json: board });
    }
    if (pathname.endsWith('/writing/join')) {
      if (env.joinHandler) return env.joinHandler(route);
      board.writing.joined = true; board.writing.participants++; board.revision++;
      return route.fulfill({ json: board });
    }
    if (pathname.endsWith('/cards')) {
      if (env.cardHandler) return env.cardHandler(route);
      return route.fulfill({ status: 500, json: { error: 'Тестовая ошибка сохранения' } });
    }
    const file = pathname === '/b/writing1234' ? 'index.html' : pathname === '/static/app.js' ? 'app.js' : pathname === '/static/style.css' ? 'style.css' : null;
    if (file) return route.fulfill({ contentType: file.endsWith('.js') ? 'application/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html', body: asset(file) });
    return route.fulfill({ status: 404 });
  });
  env.emit = data => page.evaluate(data => window.testEvents.listeners.board({ data: JSON.stringify({ ...data, participant: document.querySelector('meta[name="retro-participant"]').content }) }), data || board);
  await page.goto(origin + '/b/writing1234');
  await page.getByRole('heading', { name: board.title }).waitFor();
  return env;
}

test('Готовность: черновик, открытая правка и неотправленная карточка не завершают запись', async t => {
  const { page, sent } = await fixture(t);
  const draft = page.getByPlaceholder('Новая карточка');
  await draft.fill('Не потерять эту мысль');
  await finish(page).click();
  assert.equal(sent.length, 0);
  assert.equal(await draft.inputValue(), 'Не потерять эту мысль');
  await page.getByRole('alert').filter({ hasText: 'Сначала добавьте' }).waitFor();
  await draft.fill('');
  await page.getByRole('button', { name: 'Изменить', exact: true }).click();
  await page.locator('[data-keep="edit:card1"]').fill('Незаконченная правка');
  await finish(page).click();
  assert.equal(sent.length, 0);
  assert.equal(await page.locator('[data-keep="edit:card1"]').inputValue(), 'Незаконченная правка');
  await page.getByRole('button', { name: 'Отмена', exact: true }).click();
  await draft.fill('Карточка с ошибкой отправки');
  await page.getByRole('button', { name: 'Добавить', exact: true }).click();
  await page.getByRole('button', { name: 'Повторить отправку', exact: true }).waitFor();
  await page.reload();
  await finish(page).click();
  assert.equal(sent.length, 0, 'восстановленная очередь должна мешать завершению этапа');
  await page.getByRole('region', { name: 'Неотправленные записи' }).getByText('Карточка с ошибкой отправки', { exact: true }).waitFor();
  page.once('dialog', dialog => dialog.accept());
  await page.getByRole('button', { name: 'Удалить черновик', exact: true }).click();
  await finish(page).click();
  await resume(page).waitFor();
  assert.equal(sent.length, 1);
});

test('Готовность: ожидание HTTP переживает SSE, отмена и reload используют серверное состояние', async t => {
  const env = await fixture(t);
  const { page, board, sent } = env;
  let release, started;
  const gate = new Promise(resolve => { release = resolve; });
  const received = new Promise(resolve => { started = resolve; });
  env.readyHandler = async (route, body) => {
    started(); await gate; env.applyReady(body); await route.fulfill({ json: board });
  };
  await finish(page).click(); await received;
  board.revision++;
  await env.emit();
  assert.equal(await page.getByRole('button', { name: 'Сохраняем…', exact: true }).isDisabled(), true);
  release();
  await resume(page).waitFor();
  assert.equal(await page.getByPlaceholder('Новая карточка').count(), 0);
  await page.reload();
  await resume(page).waitFor();
  assert.equal(sent.length, 1, 'reload не отправляет готовность повторно');
  env.readyHandler = null;
  await resume(page).click();
  await finish(page).waitFor();
  assert.deepEqual(sent, [{ ready: true, round: 1, revision: 0 }, { ready: false, round: 1, revision: 1 }]);
  await page.getByPlaceholder('Новая карточка').waitFor();
});

test('Готовность: потерянный ответ сверяется чтением, старый SSE не возвращает запись после показа', async t => {
  const env = await fixture(t);
  const { page, board, sent } = env;
  const stale = structuredClone(board);
  env.readyHandler = async (route, body) => {
    env.applyReady(body);
    board.phase = 'reveal'; delete board.writing;
    await route.abort();
  };
  await finish(page).click();
  await page.getByText('Все карточки открыты. Прочитайте и дополните.', { exact: false }).waitFor();
  await env.emit(stale);
  assert.equal(await finish(page).count(), 0);
  assert.equal(await page.getByRole('region', { name: 'Готовность участников' }).count(), 0);
  assert.equal(sent.length, 1);
  await page.getByPlaceholder('Новая карточка').waitFor();
});

test('Готовность: вход без SSE регистрирует участника, сбой входа можно повторить', async t => {
  const env = await fixture(t);
  const { page, board } = env;
  board.writing.joined = false; board.revision++;
  let joins = 0;
  env.joinHandler = async route => {
    joins++;
    if (joins === 1) return route.fulfill({ status: 503, json: { error: 'Временная ошибка' } });
    board.writing.joined = true; board.writing.participants++; board.revision++;
    await route.fulfill({ json: board });
  };
  await page.reload();
  await page.getByRole('button', { name: 'Повторить подключение', exact: true }).click();
  await finish(page).waitFor();
  await page.getByText('Готовы: 0 из 3', { exact: true }).waitFor();
  assert.equal(joins, 2);
});
