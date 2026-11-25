CREATE TABLE http_request_results (
 actor TEXT NOT NULL, route TEXT NOT NULL, request_key TEXT NOT NULL,
 request_sha256 TEXT NOT NULL CHECK(length(request_sha256)=64),
 job_id TEXT NOT NULL REFERENCES jobs(id), result_json TEXT NOT NULL CHECK(json_valid(result_json)),
 created_at TEXT NOT NULL, PRIMARY KEY(actor,route,request_key)
);
