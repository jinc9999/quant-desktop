# -*- coding: utf-8 -*-
"""把指定日期段的 5m K 线按天追加到 data/{SYMBOL}.csv 尾部（只追加，不改历史）。

背景: 回测数据只到 2026-08-01，需补 8 月数据做"旧策略 vs 新策略"对照回测。
币安月度文件当月不可用，只有按天文件: data/futures/um/daily/klines/{SYM}/5m/{SYM}-5m-YYYY-MM-DD.zip
"""
import concurrent.futures
import datetime
import io
import os
import sys
import time
import urllib.request
import zipfile

DATA_DIR = r"D:\0001_ba-A - 03\quant-desktop\backtest\data"
DATES = ["2026-08-01", "2026-08-02", "2026-08-03", "2026-08-04",
         "2026-08-05", "2026-08-06", "2026-08-07", "2026-08-08",
         "2026-08-09", "2026-08-10", "2026-08-11", "2026-08-12"]
URL_TPL = ("https://data.binance.vision/data/futures/um/daily/klines/"
           "{sym}/5m/{sym}-5m-{day}.zip")
WORKERS = 12


def tail_last_ts(path):
    size = os.path.getsize(path)
    with open(path, "rb") as f:
        if size > 262144:
            f.seek(-262144, 2)  # 尾部 256KB 足够覆盖最后一行
            tail = f.read()
        else:
            tail = f.read()
    for line in reversed(tail.split(b"\n")):
        if line.strip():
            return int(line.split(b",")[0])
    return 0


def fetch_day(sym, day):
    url = URL_TPL.format(sym=sym, day=day)
    req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
    last_err = ""
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                return day, resp.read(), ""
        except Exception as e:
            last_err = f"{type(e).__name__}:{e}"
            time.sleep(2 + attempt * 3)  # 限流退避
    return day, None, last_err


def process_symbol(sym):
    path = os.path.join(DATA_DIR, sym + ".csv")
    if not os.path.exists(path):
        return sym, 0, "no-csv"
    last_ts = tail_last_ts(path)
    rows = []
    missing = []
    for day in DATES:
        day_start = int(datetime.datetime.fromisoformat(day).replace(
            tzinfo=datetime.timezone.utc).timestamp() * 1000)
        if last_ts > 0 and day_start <= last_ts:
            continue  # 该日数据已在 CSV 中，跳过下载（增量更新）
        _, blob, err = fetch_day(sym, day)
        if blob is None:
            missing.append(day + "[" + err + "]")
            continue
        try:
            zf = zipfile.ZipFile(io.BytesIO(blob))
            name = zf.namelist()[0]
            with zf.open(name) as f:
                for line in f:
                    line = line.strip()
                    if not line or not line[:1].isdigit():
                        # 跳过表头行（币安日线文件首行为 open_time,...）
                        continue
                    ts = int(line.split(b",")[0])
                    if ts > last_ts:
                        rows.append(line)
        except Exception:
            missing.append(day)
    if rows:
        with open(path, "ab") as f:
            for r in rows:
                f.write(r + b"\n")
    return sym, len(rows), (";".join(missing)[:200]) if missing else "ok"


def main():
    syms = sorted(
        fn[:-4] for fn in os.listdir(DATA_DIR)
        if fn.endswith(".csv") and fn[:-4].isascii()
    )
    print(f"共 {len(syms)} 个币，追加 {len(DATES)} 天")
    total = 0
    skipped = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as ex:
        for sym, n, status in ex.map(process_symbol, syms):
            if n:
                total += n
            if status != "ok":
                skipped += 1
                if n == 0:
                    print(f"  {sym}: 无新增（{status}）")
    print(f"完成: 新增 {total} 行，跳过/异常 {skipped} 个币")


if __name__ == "__main__":
    sys.exit(main())
