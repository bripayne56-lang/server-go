// server.js — Render-ready 1-second invisible precheck + cron ping
const http = require('http');
const fs = require('fs');
const path = require('path');
const { CronJob } = require('cron');

const PORT = process.env.PORT || 3000;
const FILE_PATH = path.join(__dirname, 'public', 'index.html');

// -------------------------
// Cron job to keep server alive (ping /precheck every 14s)
const backendUrl = `http://localhost:${PORT}/precheck`;
const job = new CronJob('*/14 * * * * *', () => { // every 14 seconds
  http.get(backendUrl, (res) => {
    if (res.statusCode === 204) {
      console.log('Server alive (status 204)');
    } else {
      console.error(`Ping returned status code: ${res.statusCode}`);
    }
  }).on('error', (err) => console.error('Ping error:', err.message));
});
job.start();

// -------------------------
// Main server
const server = http.createServer((req, res) => {

  // Only handle /precheck
  if (req.url.startsWith('/precheck')) {
    let finished = false;

    // Detect client disconnect early
    req.on('close', () => {
      if (!finished) {
        console.log('Client disconnected early — sending 204');
        res.writeHead(204);
        res.end();
        finished = true;
      }
    });

    // 1-second invisible wait
    setTimeout(() => {
      if (finished) return; // client already gone

      // Serve the page only after 1 second
      fs.readFile(FILE_PATH, (err, data) => {
        if (err) {
          res.writeHead(500);
          res.end('Error loading page');
          return;
        }

        res.writeHead(200, { 'Content-Type': 'text/html' });
        res.end(data);
        finished = true;

        const url = new URL(req.url, `http://${req.headers.host}`);
        console.log(
          'Precheck served page for:',
          url.searchParams.get('lp') || 'no lp',
          'User-Agent:',
          req.headers['user-agent']
        );
      });
    }, 1000);

    return;
  }

  // Default 404 for other paths
  res.writeHead(404);
  res.end('Not Found');
});

// Start server
server.listen(PORT, () => {
  console.log(`Server running on port ${PORT}`);
});
