const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { execFile, spawn } = require('node:child_process');
const { once } = require('node:events');
const { mkdtemp, rm } = require('node:fs/promises');
const { promisify } = require('node:util');
const { setTimeout: sleep } = require('node:timers/promises');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { chromium } = require('playwright');
const root = path.resolve(__dirname, '../..');
const { openGuest, useGuest } = require('./session.cjs');
let owner;
let directory, mock, processServer, browser, origin;
let malformed = false;
const issues = [];

before(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), 'retro-jira-integration-'));
  mock = http.createServer(async (req, res) => {
    assert.equal(req.headers.authorization, 'Bearer test-token-local-only');
    if (req.url === '/rest/api/2/myself') {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ name: 'test-user' }));
      return;
    }
    if (req.method === 'POST') {
      const chunks = []; for await (const chunk of req) chunks.push(chunk);
      const { fields } = JSON.parse(Buffer.concat(chunks));
      assert.deepEqual(fields.reporter, { name: 'test-user' });
      const issue = { key: `TEAM-${issues.length+1}`, fields };
      issues.push(issue);
      res.writeHead(201, { 'Content-Type': 'application/json' });
      res.end(malformed ? '{"key":' : JSON.stringify({ key: issue.key }));
    } else {
      const query = new URL(req.url, 'http://localhost').searchParams.get('jql');
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ issues: issues.filter(issue => query.includes(issue.fields.labels[1])) }));
    }
  });
  mock.listen(0, '127.0.0.1'); await once(mock, 'listening');
  const binary = path.join(directory, 'retro');
  await promisify(execFile)('go', ['build', '-o', binary, '.'], { cwd: root, timeout: 120000 });
  // Нулевой порт выбирает ОС; адрес берём из структурированного стартового лога.
  const env = { ...process.env };
  for (const key of Object.keys(env)) if (key.startsWith('JIRA_') || key === 'RETRO_PUBLIC_URL') delete env[key];
  Object.assign(env, { JIRA_BASE_URL: `http://127.0.0.1:${mock.address().port}`, JIRA_PROJECT_KEY: 'TEAM',
    JIRA_ISSUE_TYPE_ID: '10001', JIRA_TOKEN: 'test-token-local-only' });
  processServer = spawn(binary, ['-addr', '127.0.0.1:0', '-data', path.join(directory, 'retro.db'), '-log-json'], {
    cwd: root, env, stdio: ['ignore', 'pipe', 'pipe'],
  });
  let log = '';
  processServer.stdout.on('data', chunk => {
    log += chunk;
    for (const line of log.split('\n')) {
      try { const entry = JSON.parse(line); if (entry.msg === 'retro запущен') origin = `http://${entry.addr}`; } catch {}
    }
  });
  processServer.stderr.on('data', chunk => { log += chunk; });
  const deadline = Date.now()+15000;
  while (!origin) {
    if (Date.now() > deadline || processServer.exitCode !== null) throw new Error(`Тестовый Retro не запустился: ${log}`);
    await sleep(25);
  }
  owner = await openGuest(origin);
  browser = await chromium.launch({ headless: true,
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : { channel: 'chromium' }),
  });
}, { timeout: 180000 });

after(async () => {
  await browser?.close();
  if (processServer?.pid && processServer.exitCode === null) {
    const exited = once(processServer, 'exit'); processServer.kill('SIGTERM');
    await Promise.race([exited, sleep(10000, undefined, { ref: false })]);
    if (processServer.exitCode === null) { processServer.kill('SIGKILL'); await exited; }
  }
  if (mock) { mock.closeAllConnections(); await new Promise(resolve => mock.close(resolve)); }
  if (directory) await rm(directory, { recursive: true, force: true });
});

async function api(method, route, body) {
  const response = await fetch(origin+route, { method, headers: { 'Content-Type': 'application/json', 'X-Participant': owner.id, Cookie: owner.cookie },
    body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(10000) });
  const result = await response.json();
  assert.ok(response.ok, `Ошибка API ${response.status}: ${JSON.stringify(result)}`);
  return result;
}

async function fixture(t) {
  const board = await api('POST', '/api/boards', { title: 'Тест Jira', template: 'classic' });
  await api('POST', `/api/boards/${board.id}/phase`, { phase: 'discuss' });
  let current;
  for (const text of ['Алерты', 'Ревью', 'Оставить в Retro']) current = await api('POST', `/api/boards/${board.id}/actions`, { text, assignee: 'Анна' });
  const page = await browser.newPage(); page.setDefaultTimeout(8000); t.after(() => page.close());
  await useGuest(page.context(), origin, owner);
  await page.goto(`${origin}/b/${board.id}`);
  await page.getByText('Алерты', { exact: true }).waitFor();
  return { page, board: current };
}
const select = (page, text) => page.getByRole('checkbox', { name: `Выбрать для Jira: ${text}`, exact: true }).check();

test('Реальный сервер: выбранные решения создают задачи, ссылки переживают reload и следующее ретро', async t => {
  const { page, board } = await fixture(t);
  const initial = issues.length;
  await select(page, 'Алерты'); await select(page, 'Ревью');
  await page.getByRole('button', { name: 'Создать в Jira (2)', exact: true }).click();
  await page.getByRole('link', { name: `TEAM-${initial+2}`, exact: true }).waitFor();
  assert.equal(issues.length, initial+2);
  assert.deepEqual(issues.slice(initial).map(x => x.fields.summary), ['Алерты', 'Ревью']);
  await page.reload();
  await page.getByRole('link', { name: `TEAM-${initial+1}`, exact: true }).waitFor();
  const next = await api('POST', `/api/boards/${board.id}/next`, {});
  assert.equal(next.actions[0].jira.key, `TEAM-${initial+1}`);
  assert.equal(next.actions[2].jira, undefined);
});

test('Реальный сервер: потерянный ответ Jira восстанавливается поиском без второго POST', async t => {
  const { page } = await fixture(t);
  const initial = issues.length;
  malformed = true;
  try {
    await select(page, 'Алерты'); await page.getByRole('button', { name: 'Создать в Jira (1)', exact: true }).click();
    await page.getByRole('button', { name: 'Проверить в Jira', exact: true }).waitFor();
    await page.reload();
    await page.getByRole('button', { name: 'Проверить в Jira', exact: true }).click();
    await page.getByRole('link', { name: `TEAM-${initial+1}`, exact: true }).waitFor();
    assert.equal(issues.length, initial+1);
  } finally { malformed = false; }
});

test('Реальный сервер: потеря ответа Retro не повторяет уже созданную задачу', async t => {
  const { page, board } = await fixture(t);
  const initial = issues.length;
  await page.route(`**/actions/${board.actions[0].id}/jira`, async route => {
    const result = await route.fetch(); assert.equal(result.status(), 200); await route.abort();
  }, { times: 1 });
  await select(page, 'Алерты'); await page.getByRole('button', { name: 'Создать в Jira (1)', exact: true }).click();
  await page.getByRole('link', { name: `TEAM-${initial+1}`, exact: true }).waitFor();
  const replay = await api('POST', `/api/boards/${board.id}/actions/${board.actions[0].id}/jira`, { revision: 1 });
  assert.equal(replay.actions[0].jira.key, `TEAM-${initial+1}`);
  assert.equal(issues.length, initial+1);
});
