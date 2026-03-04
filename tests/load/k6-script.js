import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Rate } from "k6/metrics";

export const options = {
  scenarios: {
    mixed_queries: {
      executor: "ramping-vus",
      startVUs: 25,
      stages: [
        { duration: "20s", target: 200 },
        { duration: "20s", target: 500 },
        { duration: "20s", target: 500 },
      ],
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: ["p(95)<1500"],
  },
};

const baseURL = __ENV.BASE_URL || "http://localhost:8080";
const adminToken = __ENV.ADMIN_TOKEN || "";
const developerToken = __ENV.DEVELOPER_TOKEN || "";
const viewerToken = __ENV.VIEWER_TOKEN || "";

const rateLimitHits = new Counter("rate_limit_hits");
const cacheHitRate = new Rate("cache_hit_rate");

const queries = [
  {
    token: () => adminToken,
    body: {
      sql: "SELECT gh.title, gh.state FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 10",
      max_staleness_ms: 60000,
    },
  },
  {
    token: () => developerToken || adminToken,
    body: {
      sql: "SELECT gh.title, j.issue_key, j.status FROM github.pull_requests gh JOIN jira.issues j ON gh.jira_issue_id = j.issue_key WHERE gh.state = 'open' LIMIT 20",
      max_staleness_ms: 60000,
    },
  },
  {
    token: () => viewerToken || adminToken,
    body: {
      sql: "SELECT j.issue_key, j.status FROM jira.issues j LIMIT 20",
      max_staleness_ms: 120000,
    },
  },
];

export default function () {
  const query = queries[Math.floor(Math.random() * queries.length)];
  const token = query.token();
  if (!token) {
    return;
  }

  const res = http.post(`${baseURL}/v1/query`, JSON.stringify(query.body), {
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
  });

  check(res, {
    "status is 200/429": (r) => r.status === 200 || r.status === 429,
  });

  if (res.status === 429) {
    rateLimitHits.add(1);
  }

  if (res.status === 200) {
    try {
      const payload = JSON.parse(res.body);
      cacheHitRate.add(Boolean(payload.cache_hit));
    } catch (_e) {
      cacheHitRate.add(false);
    }
  }

  sleep(0.1);
}
