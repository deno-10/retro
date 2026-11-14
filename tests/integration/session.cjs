const assert = require('node:assert/strict');

async function openGuest(origin) {
  const response = await fetch(origin + '/');
  assert.equal(response.status, 200);
  const id = (await response.text()).match(/<meta name="retro-participant" content="([a-f0-9]+)">/)[1];
  const cookie = response.headers.getSetCookie()[0].split(';')[0];
  return { id, cookie };
}

async function useGuest(context, origin, guest) {
  const [name, value] = guest.cookie.split('=');
  await context.addCookies([{ name, value, url: origin, httpOnly: true, sameSite: 'Lax' }]);
}

module.exports = { openGuest, useGuest };
