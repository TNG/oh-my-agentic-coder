// Minimal local HTTP server for the Playwright smoke fixture.
// Port comes from PORT (set by playwright.config webServer.env).
const http = require('http');
const port = Number(process.env.PORT || 3456);
http.createServer((_, res) => {
  res.writeHead(200, { 'Content-Type': 'text/html' });
  res.end('<html><body>omac-playwright-smoke-ok</body></html>');
}).listen(port, '127.0.0.1', () => {
  console.log('listening on 127.0.0.1:' + port);
});
