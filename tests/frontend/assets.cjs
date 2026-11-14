const fs = require('node:fs');
const path = require('node:path');

const guestID = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';

function asset(name, participant = guestID) {
  const data = fs.readFileSync(path.resolve(__dirname, '../../web', name), 'utf8');
  return name === 'index.html'
    ? data.replace('</head>', `<meta name="retro-participant" content="${participant}"></head>`)
    : data;
}

module.exports = { asset, guestID };
