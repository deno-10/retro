const { test: nodeTest, before, after } = require('node:test');
const test = (name, fn) => nodeTest(name, { timeout: 15000 }, fn);
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const webDir = path.resolve(__dirname, '../../web');
const origin = 'http://retro.test';
let browser;

before(async () => {
  browser = await chromium.launch({
    headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}),
  });
});
after(async () => { await browser?.close(); });

async function openBoard(t) {
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  t.after(() => page.close());
  const board = {
    id: 'board1234', title: 'Тестовая доска', phase: 'reveal', isOwner: true, revision: 1,
    voteLimit: 3, votesLeft: 3, online: 1,
    columns: [{ id: 'col1', title: 'Хорошо', cards: [
      { id: 'card1', text: 'Исходная карточка', author: 'Анна', mine: true, revision: 1 },
    ] }],
    actions: [{ id: 'action1', text: 'Исходное решение', assignee: 'Анна', mine: true, done: false, revision: 1 }],
  };
  await page.addInitScript(() => {
    localStorage.setItem('retro.participant', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa');
    localStorage.setItem('retro.name', 'Анна');
    // Управляем доставкой SSE независимо от HTTP-ответов, включая их порядок.
    window.EventSource = class {
      constructor() { window.testEvents = this; this.listeners = {}; }
      addEventListener(name, listener) { this.listeners[name] = listener; }
      close() {}
    };
  });
  await page.route(`${origin}/**`, route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/boards/board1234') return route.fulfill({ json: board });
    if (url.pathname === '/static/app.js') return route.fulfill({
      contentType: 'application/javascript', body: fs.readFileSync(path.join(webDir, 'app.js'), 'utf8'),
    });
    if (url.pathname === '/static/style.css') return route.fulfill({
      contentType: 'text/css', body: fs.readFileSync(path.join(webDir, 'style.css'), 'utf8'),
    });
    if (url.pathname === '/b/board1234') return route.fulfill({
      contentType: 'text/html', body: fs.readFileSync(path.join(webDir, 'index.html'), 'utf8'),
    });
    return route.fulfill({ status: 404, json: { error: 'Неожиданный запрос' } });
  });
  await page.goto(`${origin}/b/board1234`);
  await page.getByText('Исходное решение', { exact: true }).waitFor();
  return {
    page, board,
    async emit(changes = {}) {
      Object.assign(board, changes);
      await page.evaluate(value => window.testEvents.listeners.board({ data: JSON.stringify(value) }), board);
    },
  };
}

function field(page, key) { return page.locator(`[data-keep="${key}"]`); }
async function activeField(page) { return page.evaluate(() => document.activeElement.dataset.keep); }

async function holdRequest(page, suffix) {
  let release;
  let started;
  const response = new Promise(resolve => { release = resolve; });
  const request = new Promise(resolve => { started = resolve; });
  await page.route(`${origin}/api/boards/board1234/${suffix}`, async route => {
    started(route.request().postDataJSON());
    await route.fulfill(await response);
  }, { times: 1 });
  return { request, respond: release };
}

async function editAction(page) {
  await page.locator('.action').getByRole('button', { name: 'Изменить', exact: true }).click();
}
async function editCard(page) {
  await page.locator('.card').filter({ hasText: 'Исходная карточка' })
    .getByRole('button', { name: 'Изменить', exact: true }).click();
}

// Эти сценарии проверяют пользовательские данные и фокус, а не структуру реализации.
test('SSE сохраняет оба поля редактора решения и не перехватывает фокус', async t => {
  const { page, emit } = await openBoard(t);
  await editAction(page);
  await field(page, 'action-edit:action1').fill('Новый текст решения');
  await field(page, 'action-edit-assignee:action1').fill('Борис');
  await emit({ online: 2 });
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Новый текст решения');
  assert.equal(await field(page, 'action-edit-assignee:action1').inputValue(), 'Борис');
  assert.equal(await activeField(page), 'action-edit-assignee:action1');
  await field(page, 'name').focus();
  await emit({ online: 3 });
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Новый текст решения');
  assert.equal(await field(page, 'action-edit-assignee:action1').inputValue(), 'Борис');
  assert.equal(await activeField(page), 'name');
});

test('SSE сохраняет правку карточки после перехода в другое поле', async t => {
  const { page, emit } = await openBoard(t);
  await editCard(page);
  await field(page, 'edit:card1').fill('Несохранённая правка');
  await field(page, 'name').focus();
  await emit({ online: 2 });
  assert.equal(await field(page, 'edit:card1').inputValue(), 'Несохранённая правка');
  assert.equal(await activeField(page), 'name');
});

test('Ошибочный POST карточки сохраняет новый черновик и исходную запись для повтора', async t => {
  const { page, board, emit } = await openBoard(t);
  const held = await holdRequest(page, 'cards');
  await field(page, 'draft:col1').fill('Первая карточка');
  await page.getByRole('button', { name: 'Добавить', exact: true }).click();
  const sent = await held.request;
  await field(page, 'draft:col1').fill('Вторая карточка');
  await emit({ online: 2 });
  const stayedPending = await page.getByRole('button', { name: 'Добавить', exact: true }).isDisabled();
  await field(page, 'name').fill('Борис');
  held.respond({ status: 503, json: { error: 'Ошибка отправки карточки' } });
  await page.getByText('Ошибка отправки карточки', { exact: true }).first().waitFor();
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Вторая карточка');
  assert.equal(await activeField(page), 'name');
  assert.equal(stayedPending, true);
  const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
  await recovery.getByText('Первая карточка', { exact: true }).waitFor();
  let retried;
  await page.route(`${origin}/api/boards/board1234/cards`, async route => {
    retried = route.request().postDataJSON();
    await route.fulfill({ json: board });
  }, { times: 1 });
  await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
  await recovery.getByText('Первая карточка', { exact: true }).waitFor({ state: 'detached' });
  assert.deepEqual(retried, sent);
  assert.equal(await field(page, 'draft:col1').inputValue(), 'Вторая карточка');
});

test('Ошибочный POST решения сохраняет нового ответственного и исходные данные для повтора', async t => {
  const { page, board, emit } = await openBoard(t);
  const held = await holdRequest(page, 'actions');
  await field(page, 'action-text').fill('Первое решение');
  await field(page, 'action-assignee').fill('Анна');
  await page.getByRole('button', { name: 'Записать', exact: true }).click();
  const sent = await held.request;
  await field(page, 'action-text').fill('Второе решение');
  await field(page, 'action-assignee').fill('Борис');
  await emit({ online: 2 });
  const stayedPending = await page.getByRole('button', { name: 'Записать', exact: true }).isDisabled();
  held.respond({ status: 503, json: { error: 'Ошибка отправки решения' } });
  await page.getByText('Ошибка отправки решения', { exact: true }).first().waitFor();
  assert.equal(await field(page, 'action-text').inputValue(), 'Второе решение');
  assert.equal(await field(page, 'action-assignee').inputValue(), 'Борис');
  assert.equal(await activeField(page), 'action-assignee');
  assert.equal(stayedPending, true);
  const recovery = page.getByRole('region', { name: 'Неотправленные записи' });
  await recovery.getByText('Первое решение', { exact: true }).waitFor();
  let retried;
  await page.route(`${origin}/api/boards/board1234/actions`, async route => {
    retried = route.request().postDataJSON();
    await route.fulfill({ json: board });
  }, { times: 1 });
  await recovery.getByRole('button', { name: 'Повторить отправку', exact: true }).click();
  await recovery.getByText('Первое решение', { exact: true }).waitFor({ state: 'detached' });
  assert.deepEqual(retried, sent);
  assert.equal(await field(page, 'action-text').inputValue(), 'Второе решение');
  assert.equal(await field(page, 'action-assignee').inputValue(), 'Борис');
});

test('Незавершённый PATCH решения переживает SSE без потери текста', async t => {
  const { page, emit } = await openBoard(t);
  await editAction(page);
  await field(page, 'action-edit:action1').fill('Сохраняемый текст');
  await field(page, 'action-edit-assignee:action1').fill('Борис');
  const held = await holdRequest(page, 'actions/action1');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.deepEqual(await held.request, { text: 'Сохраняемый текст', assignee: 'Борис', revision: 1 });
  await emit({ online: 2 });
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Сохраняемый текст');
  assert.equal(await field(page, 'action-edit-assignee:action1').inputValue(), 'Борис');
  assert.equal(await field(page, 'action-edit:action1').isDisabled(), true);
  assert.equal(await field(page, 'action-edit-assignee:action1').isDisabled(), true);
  held.respond({ status: 503, json: { error: 'Ошибка сохранения' } });
  await page.getByText('Ошибка сохранения', { exact: true }).waitFor();
  assert.equal(await field(page, 'action-edit:action1').inputValue(), 'Сохраняемый текст');
  assert.equal(await field(page, 'action-edit:action1').isDisabled(), false);
  assert.equal(await field(page, 'action-edit-assignee:action1').inputValue(), 'Борис');
});

test('Редактор карточки заблокирован до ответа PATCH и закрывается после сохранения', async t => {
  const { page, board, emit } = await openBoard(t);
  await editCard(page);
  await field(page, 'edit:card1').fill('Сохранённая карточка');
  const held = await holdRequest(page, 'cards/card1');
  await page.getByRole('button', { name: 'Сохранить', exact: true }).click();
  assert.deepEqual(await held.request, { text: 'Сохранённая карточка', revision: 1 });
  board.columns[0].cards[0].text = 'Сохранённая карточка';
  await emit({ online: 2 });
  assert.equal(await field(page, 'edit:card1').inputValue(), 'Сохранённая карточка');
  assert.equal(await field(page, 'edit:card1').isDisabled(), true);
  assert.equal(await page.getByRole('button', { name: 'Сохранить', exact: true }).isDisabled(), true);
  assert.equal(await page.getByRole('button', { name: 'Отмена', exact: true }).isDisabled(), true);
  held.respond({ json: board });
  await field(page, 'edit:card1').waitFor({ state: 'detached' });
  await page.getByText('Сохранённая карточка', { exact: true }).waitFor();
});

test('SSE не заменяет поле во время IME, Enter при composition не отправляет текст', async t => {
  const { page, emit } = await openBoard(t);
  let posts = 0;
  await page.route(`${origin}/api/boards/board1234/cards`, route => {
    posts++;
    return route.fulfill({ status: 503, json: { error: 'Не должно отправляться' } });
  });
  const draft = field(page, 'draft:col1');
  await draft.fill('構');
  await draft.evaluate(el => {
    window.composingElement = el;
    el.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true }));
    el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', isComposing: true, bubbles: true }));
  });
  await emit({ title: 'Обновление во время ввода' });
  assert.equal(await draft.evaluate(el => el === window.composingElement), true);
  assert.equal(await draft.inputValue(), '構');
  await draft.evaluate(el => {
    el.value = '構成';
    el.dispatchEvent(new InputEvent('input', { bubbles: true }));
    el.dispatchEvent(new CompositionEvent('compositionend', { bubbles: true }));
  });
  await page.getByRole('heading', { name: 'Обновление во время ввода', exact: true }).waitFor();
  assert.equal(await draft.inputValue(), '構成');
  assert.equal(posts, 0);
});
