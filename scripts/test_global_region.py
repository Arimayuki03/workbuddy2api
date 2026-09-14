#!/usr/bin/env python3
"""test_global_region.py — 登录后自动完善注册地区流程的契约测试（桩上游）。"""
import json, os, sys, unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import global_region

# 桩上游记录的请求 path / submit body
HITS = []

def country(code, name="", ios3="", code_num=0):
    return {"EnName": name or code, "Name": name or code, "IOS2": code,
            "IOS3": ios3 or code.lower(), "Code": str(code_num) or code}

COUNTRIES = [country("HK", "Hong Kong", code_num=852),
             country("SG", "Singapore", code_num=65),
             country("JP", "Japan", code_num=81)]

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # silence
        pass
    def _send(self, data, status=200):
        if isinstance(data, str):
            data = data.encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        HITS.append(self.path)
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n).decode()
        body = json.loads(raw) if raw else {}
        if self.path == "/billing/area/get-country-code":
            inner = json.dumps({"code": 0, "data": {"list": COUNTRIES}}, ensure_ascii=False)
            self._send(json.dumps({"code": 0, "msg": "OK", "data": inner}, ensure_ascii=False))
        elif self.path == "/billing/area/get-user-area-info":
            inner = json.dumps({"code": 0, "msg": "ok", "data": {"IOS2": "SG", "enName": "Singapore"}}, ensure_ascii=False)
            self._send(json.dumps({"code": 0, "data": inner}, ensure_ascii=False))
        elif self.path == "/console/login/account":
            HITS.append("BODY:" + json.dumps(body))  # 记录提交 body
            self._send(json.dumps({"code": 0, "msg": "OK"}))
        elif self.path == "/billing/ide/trial":
            self._send(json.dumps({"code": 0, "msg": "OK"}))
        else:
            self._send(json.dumps({"code": -1, "msg": "unknown"}))

    def do_GET(self):
        HITS.append(self.path)
        if self.path == "/auth/realms/copilot/overseas/user/register?userId=u1":
            self._send(json.dumps({"code": 200, "msg": "register success"}))
        else:
            self._send(json.dumps({"code": -1}))


class TestGlobalRegion(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.base = f"http://127.0.0.1:{cls.srv.server_address[1]}"
        t = Thread(target=cls.srv.serve_forever, daemon=True)
        t.start()
        global_region._BASE = cls.base

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def setUp(self):
        HITS.clear()

    def test_fetch_countries(self):
        ok, lst, msg = global_region.fetch_countries(intl_only=False)
        self.assertTrue(ok, msg)
        self.assertEqual(len(lst), 3)
        self.assertEqual(lst[0]["IOS2"], "HK")

    def test_fetch_countries_intl_whitelist(self):
        ok, lst, msg = global_region.fetch_countries(intl_only=True)
        self.assertTrue(ok, msg)
        codes = {c["IOS2"] for c in lst}
        self.assertEqual(codes, {"HK", "SG"})  # JP 不在国际版白名单

    def test_submit_region_shape(self):
        ok, msg = global_region.submit_region("tok", COUNTRIES[1])
        self.assertTrue(ok, msg)
        submitted = next(h for h in HITS if h.startswith("BODY:"))
        body = json.loads(submitted[len("BODY:"):])
        self.assertEqual(body["attributes"]["countryCode"], ["65"])
        self.assertEqual(body["attributes"]["countryFullName"], ["Singapore"])
        self.assertEqual(body["attributes"]["countryName"], ["SG"])

    def test_register_ok(self):
        ok, needs_region, msg = global_region.activate_region("tok", "u1")
        self.assertTrue(ok, msg)
        self.assertFalse(needs_region)

    def test_complete_flow_region_then_submit(self):
        # 模拟 register 首次 region required，提交后成功
        calls = {"n": 0}
        orig = global_region.activate_region
        def fake_activate(token, uid, base=None):
            calls["n"] += 1
            return (False, True, "region required") if calls["n"] == 1 else (True, False, "register success")
        global_region.activate_region = fake_activate
        try:
            sq = next(c for c in COUNTRIES if c["IOS2"] == "SG")
            ok, msg = global_region.complete_flow(token="tok", uid="u1", pick=sq,
                                                  call_register=True)
            self.assertTrue(ok, msg)
            self.assertTrue(any(h.startswith("/console/login/account") for h in HITS), HITS)
        finally:
            global_region.activate_region = orig

    def test_complete_flow_already_active(self):
        ok, msg = global_region.complete_flow(token="tok", uid="u1", pick=None,
                                              call_register=True)
        self.assertTrue(ok, msg)
        # 无需提交
        self.assertFalse(any(h.startswith("/console/login/account") for h in HITS), HITS)


if __name__ == "__main__":
    unittest.main()