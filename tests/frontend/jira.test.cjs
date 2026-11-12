const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const web = path.resolve(__dirname, '../../web');
const origin = 'http://retro.test';
let browser;
before(async () => {
  browser = await chromium.launch({ headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}),
  });
});
after(async () => browser?.close());

async function fixture(t, changes = {}) {
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  t.after(() => page.close());
  let board = { id: 'board1234', title: 'Ретро Jira', phase: 'discuss', revision: 1, isOwner: true,
    voteLimit: 3, votesLeft: 3, jira: { project: 'TEAM', issueType: '10001' },
    columns: [{ id: 'col1', title: 'Хорошо', cards: [] }],
    actions: ['Настроить алерты', 'Ускорить ревью', 'Не отправлять'].map((text, i) => ({ id: `action${i}`, text, assignee: 'Анна', revision: 1, mine: true, done: false })),
    ...changes,
  };
  const sent = [];
  let handler = async (route, actionId, check) => {
    sent.push({ actionId, check, body: route.request().postDataJSON() });
    const action = board.actions.find(a => a.id === actionId);
    action.jira = { status: 'created', key: `TEAM-${sent.length}`, url: `https://jira.example.org/browse/TEAM-${sent.length}` };
    action.revision++; board.revision++;
    await route.fulfill({ json: board });
  };
  await page.addInitScript(() => {
    localStorage.setItem('retro.participant', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa');
    window.EventSource = class {
      constructor() { window.testEvents = this; this.listeners = {}; }
      addEventListener(name, fn) { this.listeners[name] = fn; }
      close() {}
    };
  });
  await page.route(`${origin}/**`, async route => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname === '/api/boards/board1234') return route.fulfill({ json: board });
    const match = pathname.match(/\/actions\/(action\d)\/jira(\/check)?$/);
    if (match) return handler(route, match[1], !!match[2]);
    const file = pathname === '/b/board1234' ? 'index.html' : pathname === '/static/app.js' ? 'app.js' : pathname === '/static/style.css' ? 'style.css' : null;
    if (file) return route.fulfill({ contentType: file.endsWith('.js') ? 'application/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(web, file)) });
    return route.fulfill({ status: 404, body: '' });
  });
  await page.goto(origin+'/b/board1234');
  await page.getByText('Настроить алерты', { exact: true }).waitFor();
  return { page, sent, get board() { return board; }, set handler(value) { handler = value; },
    emit: () => page.evaluate(data => window.testEvents.listeners.board({ data: JSON.stringify(data) }), board) };
}
const select = (page, name) => page.getByRole('checkbox', { name: `Выбрать для Jira: ${name}`, exact: true });
const send = page => page.getByRole('button', { name: /^Создать в Jira/ });

test('Jira: отправляются только выбранные решения, отметка выполнения не меняется', async t => {
  const { page, sent } = await fixture(t);
  await select(page, 'Настроить алерты').check();
  await select(page, 'Ускорить ревью').check();
  await send(page).click();
  await page.getByRole('link', { name: 'TEAM-2', exact: true }).waitFor();
  assert.deepEqual(sent.map(x => x.actionId), ['action0', 'action1']);
  assert.deepEqual(sent.map(x => x.body), [{ revision: 1 }, { revision: 1 }]);
  assert.equal(await page.getByRole('checkbox', { name: 'Не сделано', exact: true }).count(), 3);
  assert.equal(await page.getByRole('link', { name: 'TEAM-1', exact: true }).getAttribute('href'), 'https://jira.example.org/browse/TEAM-1');
  await page.reload();
  await page.getByRole('link', { name: 'TEAM-2', exact: true }).waitFor();
  assert.equal(sent.length, 2);
});

test('Jira: частичный отказ сохраняет успешные ссылки и разрешает повторить только отказ', async t => {
  const env = await fixture(t); const { page } = env;
  const ids = [];
  env.handler = async (route, id) => {
    ids.push(id);
    const action = env.board.actions.find(a => a.id === id);
    action.jira = ids.length === 2 ? { status: 'failed', error: 'Jira отказала в доступе' } : { status: 'created', key: `TEAM-${ids.length}`, url: `https://jira.example.org/browse/TEAM-${ids.length}` };
    action.revision++; env.board.revision++;
    await route.fulfill({ json: env.board });
  };
  await select(page, 'Настроить алерты').check(); await select(page, 'Ускорить ревью').check(); await send(page).click();
  await page.getByText('Jira отказала в доступе', { exact: true }).waitFor();
  await select(page, 'Ускорить ревью').check(); await send(page).click();
  await page.getByRole('link', { name: 'TEAM-3', exact: true }).waitFor();
  assert.deepEqual(ids, ['action0', 'action1', 'action1']);
});

test('Jira: неизвестный результат после reload проверяется без повторного создания', async t => {
  const env = await fixture(t); const { page } = env;
  const calls = [];
  env.handler = async (route, id, check) => {
    calls.push(check);
    env.board.actions[0].jira = check
      ? { status: 'created', key: 'TEAM-42', url: 'https://jira.example.org/browse/TEAM-42' }
      : { status: 'unknown', error: 'Результат неизвестен', searchUrl: 'https://jira.example.org/issues/?jql=labels%3Dretro-test' };
    env.board.actions[0].revision++; env.board.revision++;
    await route.fulfill({ json: env.board });
  };
  await select(page, 'Настроить алерты').check(); await send(page).click();
  await page.getByText('Результат неизвестен', { exact: true }).waitFor();
  await page.reload();
  await page.getByRole('button', { name: 'Проверить в Jira', exact: true }).click();
  await page.getByRole('link', { name: 'TEAM-42', exact: true }).waitFor();
  assert.deepEqual(calls, [false, true]);
});

test('Jira: потеря ответа обновляет состояние и не отправляет остальные пункты', async t => {
  const env = await fixture(t); const { page } = env; let calls = 0;
  env.handler = async route => {
    calls++;
    env.board.actions[0].jira = { status: 'created', key: 'TEAM-7', url: 'https://jira.example.org/browse/TEAM-7' };
    env.board.actions[0].revision++; env.board.revision++;
    await route.abort();
  };
  await select(page, 'Настроить алерты').check(); await select(page, 'Ускорить ревью').check(); await send(page).click();
  await page.getByRole('link', { name: 'TEAM-7', exact: true }).waitFor();
  assert.equal(calls, 1);
  assert.equal(await select(page, 'Ускорить ревью').isChecked(), true);
});

test('Jira: SSE сохраняет выбор, изменение выбранного текста требует нового выбора', async t => {
  const env = await fixture(t); const { page } = env;
  await select(page, 'Настроить алерты').check();
  env.board.revision++; env.board.actions[1].text = 'Правка другой карточки'; await env.emit();
  assert.equal(await select(page, 'Настроить алерты').isChecked(), true);
  env.board.revision++; env.board.actions[0].revision++; env.board.actions[0].text = 'Обновить алерты'; await env.emit();
  assert.equal(await select(page, 'Обновить алерты').isChecked(), false);
  assert.equal(await send(page).isDisabled(), true);
});

test('Jira: нет отправки без подключения, прав ведущего или фазы обсуждения', async t => {
  for (const changes of [{ jira: null }, { isOwner: false }, { phase: 'reveal' }, { nextId: 'следующее' }]) {
    const { page } = await fixture(t, changes);
    assert.equal(await page.getByRole('checkbox', { name: /^Выбрать для Jira:/ }).count(), 0);
    assert.equal(await send(page).count(), 0);
  }
});

test('Jira: ссылка из импортированных данных не выполняет JavaScript', async t => {
  const env = await fixture(t);
  env.board.revision++; env.board.actions[0].jira = { status: 'created', key: 'TEAM-1', url: 'javascript:alert(1)' };
  await env.emit();
  assert.equal(await env.page.getByRole('link', { name: 'TEAM-1' }).count(), 0);
});
