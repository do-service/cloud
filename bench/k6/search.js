// R5: name search across a user's whole tree, p95 < 300 ms at 20M nodes.
// Queries mix words most users have hundreds of (the worst case: many
// matches to sort) with rarer ones and misses.
import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL, users } from './common.js';

const COOKIE_NAME = __ENV.COOKIE_NAME || 'session';
const QUERIES = ['отчёт', 'договор', 'фото', 'report', 'invoice', 'смета', 'roadmap', 'март', 'ёлка', 'сверки', 'readme', 'нет такого'];

export const options = {
  scenarios: {
    search: { executor: 'constant-vus', vus: Number(__ENV.VUS || 50), duration: __ENV.DURATION || '1m' },
  },
  thresholds: {
    'http_req_duration{name:search}': ['p(95)<300'],
    'checks': ['rate==1'],
  },
};

export default function () {
  const user = users[Math.floor(Math.random() * users.length)];
  const q = QUERIES[Math.floor(Math.random() * QUERIES.length)];
  const res = http.get(`${BASE_URL}/api/v1/search?q=${encodeURIComponent(q)}`, {
    headers: { Cookie: `${COOKIE_NAME}=${user.token}` },
    tags: { name: 'search' },
  });
  check(res, { 'status 200': (r) => r.status === 200 });
}
