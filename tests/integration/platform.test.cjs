const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { execFile, spawn } = require('node:child_process');
const { once } = require('node:events');
const { createServer, request } = require('node:http');
const { mkdtemp, rm } = require('node:fs/promises');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { promisify } = require('node:util');
const { setTimeout: sleep } = require('node:timers/promises');
const { chromium } = require('playwright');

let directory, service, gateway, browser, origin, upstream;
let serverLog = '';
const prefix = '/team/retro';
const users = { alice: '00000000-1111-2222-3333-444444444444', bob: '00000000-1111-2222-3333-555555555555' };
const listen = async server => { server.listen(0, '127.0.0.1'); await once(server, 'listening'); return server.address().port; };

before(async () => {
  directory = await mkdtemp(path.join(tmpdir(), 'retro-baas-'));
  const binary = path.join(directory, 'retro');
  await promisify(execFile)('go', ['build', '-o', binary, '.'], { cwd: path.resolve(__dirname, '../..'), timeout: 120000 });
  const reservation = createServer();
  const port = await listen(reservation);
  await new Promise(resolve => reservation.close(resolve));
  upstream = `http://127.0.0.1:${port}`;
  // Шлюз существует только в тесте: cookie выбирает фиктивную учётную запись,
  // клиентские X-BaaS-* удаляются. Настоящий вход выполняет BaaS, не Retro.
  gateway = createServer((req, res) => {
    const user = /retro-test-user=bob/.test(req.headers.cookie || '') ? users.bob : users.alice;
    if (!req.url.startsWith(prefix + '/')) { res.writeHead(404).end(); return; }
    const headers = { ...req.headers };
    for (const key of Object.keys(headers)) if (key.startsWith('x-baas-')) delete headers[key];
    headers['x-baas-user-id'] = user;
    headers['x-baas-space-name'] = 'retro-test';
    // Host upstream отличается от публичного origin, как бывает за ingress.
    headers.host = `127.0.0.1:${port}`;
    const outgoing = request(upstream + req.url.slice(prefix.length), { method: req.method, headers }, incoming => {
      res.writeHead(incoming.statusCode, incoming.headers); incoming.pipe(res);
    });
    outgoing.on('error', () => { if (!res.headersSent) res.writeHead(502); res.end(); });
    res.on('close', () => outgoing.destroy());
    req.pipe(outgoing);
  });
  origin = `http://127.0.0.1:${await listen(gateway)}`;
  service = spawn(binary, [], {
    stdio: ['ignore', 'pipe', 'pipe'],
    env: {
      ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('RETRO_') && !key.startsWith('JIRA_'))),
      RETRO_ADDR: `127.0.0.1:${port}`, RETRO_DATA: path.join(directory, 'retro.db'),
      RETRO_AUTH_MODE: 'baas', RETRO_BAAS_SPACE: 'retro-test', RETRO_TRUSTED_PROXY: '127.0.0.1/32',
      RETRO_PUBLIC_ORIGIN: origin, RETRO_BASE_PATH: prefix,
    },
  });
  service.stdout.on('data', chunk => { serverLog = (serverLog + chunk).slice(-16000); });
  service.stderr.on('data', chunk => { serverLog = (serverLog + chunk).slice(-16000); });
  const deadline = Date.now() + 15000;
  for (;;) {
    if (service.exitCode !== null) throw new Error(serverLog);
    try { if ((await fetch(upstream + '/readyz')).ok) break; } catch {}
    if (Date.now() > deadline) throw new Error(serverLog);
    await sleep(50);
  }
  browser = await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}) });
}, { timeout: 180000 });

after(async () => {
  await browser?.close();
  if (service?.pid && service.exitCode === null) { const exited = once(service, 'exit'); service.kill('SIGTERM'); await exited; }
  if (gateway) { gateway.closeAllConnections(); await new Promise(resolve => gateway.close(resolve)); }
  if (directory) await rm(directory, { recursive: true, force: true });
});

test('BaaS: префикс, стабильная личность, SSE, смена аккаунта и изоляция черновиков', { timeout: 30000 }, async t => {
  const context = await browser.newContext(); t.after(() => context.close());
  const page = await context.newPage(); page.setDefaultTimeout(8000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto(origin + prefix + '/');
  const aliceID = await page.locator('meta[name="retro-participant"]').getAttribute('content');
  assert.match(aliceID, /^[a-f0-9]{64}$/);
  assert.equal((await fetch(upstream + '/api/templates')).status, 401);
  const created = await page.evaluate(async prefix => {
    const response = await fetch(prefix + '/api/boards', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: 'Корпоративное ретро' }) });
    return { status: response.status, body: await response.json() };
  }, prefix);
  assert.equal(created.status, 201);
  const id = created.body.id;
  await page.goto(origin + prefix + '/b/' + id);
  await page.getByRole('button', { name: 'Я закончил', exact: true }).waitFor();
  assert.equal(await page.getByRole('link', { name: 'Авито · Ретро — на главную' }).getAttribute('href'), prefix + '/');
  const input = page.getByPlaceholder('Новая карточка').first();
  await input.fill('Личный черновик Алисы');
  // Проверяем, что обычная клиентская отправка использует путь компонента.
  await page.getByRole('button', { name: 'Добавить', exact: true }).first().click();
  await page.getByRole('main').getByText('Личный черновик Алисы', { exact: true }).waitFor();
  await input.fill('Несохранённый текст Алисы');
  // Новый браузер той же учётной записи получает того же ведущего без localStorage.
  const other = await browser.newContext(); t.after(() => other.close());
  const sameUser = await other.newPage();
  await sameUser.goto(origin + prefix + '/b/' + id);
  assert.equal(await sameUser.locator('meta[name="retro-participant"]').getAttribute('content'), aliceID);
  await sameUser.getByRole('main').getByText('Личный черновик Алисы', { exact: true }).waitFor();
  // Смена cookie не превращает старый черновик в запись нового пользователя.
  await context.addCookies([{ name: 'retro-test-user', value: 'bob', url: origin }]);
  const rejected = await page.evaluate(async ({ prefix, id, aliceID, columnId }) => {
    const response = await fetch(`${prefix}/api/boards/${id}/cards`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Participant': aliceID }, body: JSON.stringify({ columnId, text: 'Нельзя отправлять', author: '' }) });
    return response.status;
  }, { prefix, id, aliceID, columnId: created.body.columns[0].id });
  assert.equal(rejected, 409);
  assert.equal((await page.reload()).status(), 404);
  await page.getByRole('heading', { name: 'Доска недоступна' }).waitFor();
  const bobID = await page.locator('meta[name="retro-participant"]').getAttribute('content');
  assert.notEqual(bobID, aliceID);
  for (const suffix of ['', '/history', '/export.md', '/events']) {
    const denied = await context.request.get(origin + prefix + '/api/boards/' + id + suffix, { timeout: 3000 });
    assert.equal(denied.status(), 404, suffix);
    assert.doesNotMatch(await denied.text(), /Корпоративное ретро|Личный черновик Алисы/);
  }
  sameUser.once('dialog', dialog => dialog.accept(bobID));
  await sameUser.getByRole('button', { name: 'Добавить участника' }).click();
  await sameUser.getByText('Доступ открыт.', { exact: false }).waitFor();
  assert.equal((await page.reload()).status(), 200);
  await page.getByRole('button', { name: 'Я закончил', exact: true }).waitFor();
  assert.equal(await page.getByPlaceholder('Новая карточка').first().inputValue(), '');
  assert.equal(await page.getByText('Несохранённый текст Алисы', { exact: true }).count(), 0);
  await context.addCookies([{ name: 'retro-test-user', value: 'alice', url: origin }]);
  await page.reload();
  await page.getByRole('button', { name: 'Я закончил', exact: true }).waitFor();
  assert.equal(await page.getByPlaceholder('Новая карточка').first().inputValue(), 'Несохранённый текст Алисы');
  assert.deepEqual(errors, []);
});
