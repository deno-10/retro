const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { asset, guestID } = require('./assets.cjs');

let browser;
before(async () => {
  browser = await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
    ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}) });
});
after(async () => { await browser?.close(); });

test('Ключ из browser storage не даёт прав, URL потока не содержит ID', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const forged = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';
  await page.addInitScript(id => {
    localStorage.setItem('retro.participant', id);
    sessionStorage.setItem('retro.participant', id);
    window.EventSource = class {
      constructor(url) { window.stream = this; this.url = url; this.listeners = {}; }
      addEventListener(name, listener) { this.listeners[name] = listener; }
      close() { this.closed = true; }
    };
  }, forged);
  const claims = [];
  const board = { id:'board1234', title:'Тестовая доска', phase:'reveal', isOwner:false,
    revision:1, voteLimit:3, votesLeft:3, columns:[{ id:'col1', title:'Колонка', cards:[] }], actions:[] };
  await page.route('http://retro.test/**', route => {
    const name = new URL(route.request().url()).pathname;
    if (name === '/api/boards/board1234') {
      claims.push(route.request().headers()['x-participant']);
      return route.fulfill({ json:board });
    }
    if (name.startsWith('/api/')) return route.fulfill({json:[]});
    const file = name === '/b/board1234' ? 'index.html' : name.split('/').pop();
    if (!['index.html','app.js','style.css'].includes(file)) return route.fulfill({ status:404 });
    return route.fulfill({ contentType: {'app.js':'application/javascript','style.css':'text/css','index.html':'text/html'}[file], body:asset(file) });
  });
  await page.goto('http://retro.test/b/board1234');
  const editor = page.getByPlaceholder('Новая карточка').first();
  await editor.fill('Черновик');
  await page.reload();
  assert.equal(await editor.inputValue(), 'Черновик');
  assert.ok(claims.length >= 2 && claims.every(id => id === guestID));
  assert.equal(await page.evaluate(() => window.stream.url), '/api/boards/board1234/events');
  await page.evaluate(({board, forged}) => {
    window.stream.listeners.board({ data:JSON.stringify({ ...board, participant:forged, title:'Чужое событие', revision:99 }) });
  }, {board,forged});
  assert.equal(await page.evaluate(() => window.stream.closed), true);
  assert.equal(await page.getByText('Чужое событие').count(),0);
  assert.equal(await editor.inputValue(),'Черновик');
});
