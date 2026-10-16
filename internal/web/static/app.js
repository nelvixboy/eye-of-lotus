/* ==========================================================================
   Eye of Lotus - the whole interface

   No framework, no build step, no dependency, no external request. The
   page asks the server for a snapshot every couple of seconds and patches
   the numbers that changed. Everything else is drawn once and left alone,
   because a panel that rebuilds itself is a panel that flickers.
   ========================================================================== */

(function () {
  "use strict";

  var root = document.documentElement;
  var BASE = (root.dataset.base || "").replace(/\/$/, "");
  var VERSION = root.dataset.version || "dev";

  var POLL_MS = 2000;
  var HISTORY_MS = 60000;      // history is refetched once a minute
  var STALE_AFTER = 12000;     // a snapshot older than this is called stale

  var state = {
    session: null,
    snap: null,
    meta: null,
    history: { range: "1h", points: [] },
    receivedAt: 0,
    failures: 0,
    timer: null
  };

  /* ---------------------------------------------------------------- utils */

  function $(id) { return document.getElementById(id); }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  }

  function clear(node) { while (node && node.firstChild) node.removeChild(node.firstChild); }

  function num(v, digits) {
    if (v === null || v === undefined || isNaN(v)) return "-";
    var d = digits === undefined ? 0 : digits;
    return Number(v).toFixed(d);
  }

  function pct(v, digits) { return num(v, digits === undefined ? 1 : digits) + "%"; }

  function bytes(v) {
    if (!v && v !== 0) return "-";
    if (v < 1024) return v + " B";
    var units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
    var x = v, i = -1;
    while (x >= 1024 && i < units.length - 1) { x /= 1024; i++; }
    return x.toFixed(x < 10 ? 1 : 0) + " " + units[i];
  }

  function rate(v) { return bytes(v) + "/s"; }

  function count(v) { return num(v, 0).replace(/\B(?=(\d{3})+(?!\d))/g, " "); }

  function duration(seconds) {
    if (!seconds && seconds !== 0) return "-";
    var s = Math.floor(seconds);
    var d = Math.floor(s / 86400);
    var h = Math.floor((s % 86400) / 3600);
    var m = Math.floor((s % 3600) / 60);
    if (d > 0) return d + "d " + h + "h";
    if (h > 0) return h + "h " + m + "m";
    if (m > 0) return m + "m " + (s % 60) + "s";
    return s + "s";
  }

  function ago(ms) {
    if (!ms) return "-";
    var diff = Date.now() - ms;
    if (diff < 0) return "now";
    return duration(diff / 1000) + " ago";
  }

  function severityRank(s) { return s === "crit" ? 3 : s === "warn" ? 2 : s === "info" ? 1 : 0; }

  function severityOfRatio(value, warn, crit) {
    if (value >= crit) return "crit";
    if (value >= warn) return "warn";
    return "ok";
  }

  function mark(sev) {
    var m = el("span", "mark");
    m.dataset.sev = sev || "ok";
    m.setAttribute("aria-hidden", "true");
    return m;
  }

  function card(title, aside, extraClass) {
    var c = el("div", "card" + (extraClass ? " " + extraClass : ""));
    if (title) {
      var head = el("div", "card__head");
      head.appendChild(el("h3", "card__title", title));
      if (aside) head.appendChild(el("span", "card__aside", aside));
      c.appendChild(head);
    }
    return c;
  }

  function kvRow(dl, key, value, strong) {
    dl.appendChild(el("dt", null, key));
    var dd = el("dd");
    if (strong) dd.appendChild(el("strong", null, value));
    else dd.textContent = value;
    dl.appendChild(dd);
    return dd;
  }

  function bar(value, warn, crit) {
    var sev = severityOfRatio(value, warn, crit);
    var wrap = el("div", "bar" + (sev === "warn" ? " bar--warn" : sev === "crit" ? " bar--crit" : ""));
    var fill = el("div", "bar__fill");
    fill.style.width = Math.max(0, Math.min(100, value)) + "%";
    wrap.appendChild(fill);
    return wrap;
  }

  function tile(label, value, sub, ratio, warn, crit) {
    var c = card(null, null, "tile");
    c.appendChild(el("div", "tile__label", label));
    var v = el("div", "tile__value");
    v.appendChild(document.createTextNode(value));
    if (sub) {
      var s = el("small");
      s.textContent = " " + sub;
      v.appendChild(s);
    }
    c.appendChild(v);
    if (ratio !== undefined && ratio !== null) c.appendChild(bar(ratio, warn, crit));
    return c;
  }

  function emptyNote(text) {
    return el("div", "empty", text);
  }

  /* --------------------------------------------------------------- charts */

  /* sparkline draws a thin polyline over a fixed viewBox. No fill, no
     gradient, no smoothing: one stroke, flat colour. */
  function sparkline(values, options) {
    var opt = options || {};
    var w = 300, h = 70, pad = 2;
    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 " + w + " " + h);
    svg.setAttribute("preserveAspectRatio", "none");
    svg.setAttribute("class", "chart");

    // A single gridline at the threshold, when there is one.
    if (opt.threshold !== undefined && opt.max > 0) {
      var ty = h - pad - (opt.threshold / opt.max) * (h - pad * 2);
      var grid = document.createElementNS(svg.namespaceURI, "line");
      grid.setAttribute("x1", 0); grid.setAttribute("x2", w);
      grid.setAttribute("y1", ty); grid.setAttribute("y2", ty);
      grid.setAttribute("class", "chart__threshold");
      svg.appendChild(grid);
    }

    if (!values || values.length < 2) return svg;

    var max = opt.max || Math.max.apply(null, values) || 1;
    var min = opt.min || 0;
    var step = w / (values.length - 1);
    var points = [];
    for (var i = 0; i < values.length; i++) {
      var v = values[i];
      if (v === null || v === undefined || isNaN(v)) continue;
      var clamped = Math.max(min, Math.min(max, v));
      var y = h - pad - ((clamped - min) / (max - min)) * (h - pad * 2);
      points.push((i * step).toFixed(2) + "," + y.toFixed(2));
    }
    var line = document.createElementNS(svg.namespaceURI, "polyline");
    line.setAttribute("points", points.join(" "));
    line.setAttribute("class", "chart__line" + (opt.cls ? " " + opt.cls : ""));
    svg.appendChild(line);
    return svg;
  }

  function chartCard(title, aside, values, options) {
    var c = card(title, aside);
    var frame = el("div", "chart__frame");
    frame.appendChild(sparkline(values, options));
    c.appendChild(frame);
    if (options && options.legend) {
      var lg = el("div", "chart__legend");
      options.legend.forEach(function (item) {
        var span = el("span");
        span.appendChild(el("i"));
        span.appendChild(document.createTextNode(item));
        lg.appendChild(span);
      });
      c.appendChild(lg);
    }
    return c;
  }

  /* --------------------------------------------------------------- network */

  function api(path, options) {
    var opt = options || {};
    return fetch(BASE + path, {
      method: opt.method || "GET",
      headers: opt.body ? { "Content-Type": "application/json" } : undefined,
      body: opt.body ? JSON.stringify(opt.body) : undefined,
      credentials: "same-origin",
      cache: "no-store"
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (body) {
        if (!res.ok) {
          var err = new Error((body && body.error) || ("HTTP " + res.status));
          err.status = res.status;
          err.body = body;
          throw err;
        }
        return body;
      });
    });
  }

  /* ------------------------------------------------------------ rendering */

  function renderMasthead(snap, meta) {
    $("m-host").textContent = (snap.host && snap.host.hostname ? snap.host.hostname : "unknown") +
      (snap.host && snap.host.os ? " / " + snap.host.os : "");

    var verdict = $("m-verdict");
    var counts = countIssues(snap.issues || []);
    verdict.dataset.sev = snap.verdict || "ok";
    verdict.textContent = verdictWord(snap.verdict, counts);

    var stale = $("m-stale");
    if (stale) stale.remove();
    if (state.receivedAt && Date.now() - state.receivedAt > STALE_AFTER) {
      var s = el("span", "stale", "STALE");
      s.id = "m-stale";
      $("m-clock").parentNode.insertBefore(s, $("m-clock"));
    }
    $("m-clock").textContent = new Date().toLocaleTimeString();
  }

  function countIssues(issues) {
    var out = { crit: 0, warn: 0, info: 0 };
    issues.forEach(function (i) {
      if (out[i.severity] !== undefined) out[i.severity]++;
    });
    return out;
  }

  function verdictWord(sev, counts) {
    if (sev === "crit") return "CRITICAL " + counts.crit;
    if (sev === "warn") return "WARNING " + counts.warn;
    if (sev === "info") return "NOTICE " + counts.info;
    return "AT PEACE";
  }

  function renderVerdict(snap) {
    var grid = $("verdict-grid");
    clear(grid);
    var issues = snap.issues || [];
    var counts = countIssues(issues);

    grid.appendChild(tile("Findings", count(counts.crit + counts.warn + counts.info),
      counts.crit ? counts.crit + " critical" : (counts.warn ? counts.warn + " warning" : "all clear"),
      null));

    grid.appendChild(tile("Uptime", duration(snap.uptime), "since boot", null));
    grid.appendChild(tile("Processes", count(snap.total_procs), "running", null));
    grid.appendChild(tile("Collection", num(snap.collect_ms, 1) + " ms", "per sample", snap.collect_ms, 250, 800));

    var worst = issues.length && issues[0].severity === "crit" ? "crit" :
      (issues.length && issues[0].severity === "warn" ? "warn" : "ok");
    var card0 = card("First word", snap.verdict === "ok" ? "nothing to report" : "read this first", worst === "crit" ? "card--crit" : worst === "warn" ? "card--warn" : "");
    card0.classList.add("card--span");
    if (!issues.length) {
      card0.appendChild(emptyNote("The eye is open and nothing is wrong."));
    } else {
      card0.appendChild(findingNode(issues[0]));
      if (issues.length > 1) {
        card0.appendChild(el("div", "card__note", (issues.length - 1) + " more finding(s) in section V."));
      }
    }
    grid.appendChild(card0);
  }

  function findingNode(issue) {
    var n = el("div", "finding finding--" + issue.severity);
    n.appendChild(mark(issue.severity));
    var body = el("div", "finding__body");
    body.appendChild(el("div", "finding__title", issue.title));
    if (issue.detail) body.appendChild(el("div", "finding__detail", issue.detail));
    if (issue.hint) body.appendChild(el("div", "finding__hint", "\u21B3 " + issue.hint));
    var foot = el("div", "finding__id", issue.id + (issue.subject ? " \u00B7 " + issue.subject : ""));
    if (issue.value) {
      foot.textContent += " \u00B7 " + num(issue.value, 1) + (issue.threshold ? " / " + num(issue.threshold, 1) : "");
    }
    body.appendChild(foot);
    n.appendChild(body);
    return n;
  }

  function renderHardware(snap) {
    var grid = $("hardware-grid");
    clear(grid);

    var cpu = snap.cpu || {};
    var mem = snap.memory || {};
    var cpuSev = severityOfRatio(cpu.usage || 0, 75, 92);
    var cpuCard = card("Processor", cpu.model ? cpu.model : (snap.host.cpus + " logical processors"),
      cpuSev === "crit" ? "card--crit" : cpuSev === "warn" ? "card--warn" : "");
    var cpuDl = el("dl", "kv");
    kvRow(cpuDl, "usage", pct(cpu.usage, 1), true);
    kvRow(cpuDl, "iowait", pct(cpu.iowait, 1));
    if (cpu.steal > 0) kvRow(cpuDl, "steal", pct(cpu.steal, 1));
    kvRow(cpuDl, "load 1 / 5 / 15", num(cpu.load1, 2) + " / " + num(cpu.load5, 2) + " / " + num(cpu.load15, 2));
    kvRow(cpuDl, "clock", cpu.mhz ? num(cpu.mhz, 2) + " GHz" : "-");
    kvRow(cpuDl, "cores", (cpu.cores || []).length);
    cpuCard.appendChild(cpuDl);
    cpuCard.appendChild(bar(cpu.usage || 0, 75, 92));

    (cpu.cores || []).forEach(function (v, i) {
      var m = el("div", "meter");
      var row = el("div", "meter__row");
      row.appendChild(el("span", "meter__name", "core " + i));
      row.appendChild(el("span", "meter__value", pct(v, 0)));
      m.appendChild(row);
      m.appendChild(bar(v, 75, 92));
      cpuCard.appendChild(m);
    });
    grid.appendChild(cpuCard);

    var memSev = severityOfRatio(mem.used_pct || 0, 90, 96);
    var memCard = card("Memory", bytes(mem.total), memSev === "crit" ? "card--crit" : memSev === "warn" ? "card--warn" : "");
    var memDl = el("dl", "kv");
    kvRow(memDl, "used", bytes(mem.used) + "  (" + pct(mem.used_pct, 1) + ")", true);
    kvRow(memDl, "available", bytes(mem.available));
    kvRow(memDl, "free", bytes(mem.free));
    kvRow(memDl, "cache + buffers", bytes((mem.cached || 0) + (mem.buffers || 0)));
    kvRow(memDl, "swap", mem.swap_total ? bytes(mem.swap_used) + " / " + bytes(mem.swap_total) + "  (" + pct(mem.swap_pct, 1) + ")" : "none");
    memCard.appendChild(memDl);
    memCard.appendChild(bar(mem.used_pct || 0, 90, 96));
    if (snap.temp_c > 0) {
      var tSev = severityOfRatio(snap.temp_c, 80, 95);
      var tRow = el("div", "meter");
      var tTop = el("div", "meter__row");
      tTop.appendChild(el("span", "meter__name", "temperature"));
      tTop.appendChild(el("span", "meter__value", num(snap.temp_c, 1) + " \u00B0C"));
      tRow.appendChild(tTop);
      tRow.appendChild(bar(snap.temp_c, 80, 95));
      if (tSev !== "ok") tRow.querySelector(".bar").classList.add(tSev === "crit" ? "bar--crit" : "bar--warn");
      memCard.appendChild(tRow);
    }
    grid.appendChild(memCard);

    var ioRead = (state.history.points || []).map(function (p) { return p.disk_read_bps; });
    var ioWrite = (state.history.points || []).map(function (p) { return p.disk_write_bps; });
    var ioMax = Math.max(1, Math.max.apply(null, ioRead.concat(ioWrite)));
    var ioCard = chartCard("Storage throughput", state.history.range + " window", ioRead,
      { max: ioMax, legend: ["read: " + rate(lastOf(ioRead)), "write: " + rate(lastOf(ioWrite))] });
    grid.appendChild(ioCard);

    if (!(snap.disks || []).length) {
      var none = card("Filesystems", "none measured");
      none.appendChild(emptyNote("No real filesystem is mounted, or /proc/mounts is unreadable."));
      grid.appendChild(none);
      return;
    }

    (snap.disks || []).forEach(function (d) {
      var sev = severityOfRatio(d.used_pct || 0, 88, 95);
      var c = card(d.mount, d.fstype + " \u00B7 " + d.device, sev === "crit" ? "card--crit" : sev === "warn" ? "card--warn" : "");
      var dl = el("dl", "kv");
      kvRow(dl, "used", bytes(d.used) + " of " + bytes(d.total), true);
      kvRow(dl, "free", bytes(d.free));
      kvRow(dl, "inodes", pct(d.inodes_pct, 1));
      if (d.read_bps || d.write_bps) {
        kvRow(dl, "i/o", "r " + rate(d.read_bps) + "  w " + rate(d.write_bps));
      }
      if (d.read_only) kvRow(dl, "mode", "read-only");
      c.appendChild(dl);
      c.appendChild(bar(d.used_pct || 0, 88, 95));
      grid.appendChild(c);
    });
  }

  function lastOf(arr) {
    if (!arr || !arr.length) return 0;
    return arr[arr.length - 1] || 0;
  }

  function renderNetwork(snap) {
    var grid = $("network-grid");
    clear(grid);
    var net = snap.net || {};

    var online = net.online;
    var c = card("Reachability", net.checked_via || "", online ? "" : "card--crit");
    var dl = el("dl", "kv");
    kvRow(dl, "internet", online ? "reachable" : "unreachable", true);
    kvRow(dl, "probe", net.checked_via || "-");
    kvRow(dl, "latency", net.latency_ms ? num(net.latency_ms, 1) + " ms" : "-");
    kvRow(dl, "dns", (net.dns_ok ? "resolves " + (net.dns_name || "") + " in " + num(net.dns_ms, 0) + " ms" : "FAILING"));
    kvRow(dl, "received", bytes(net.rx_total) + " (" + rate(net.rx_bps) + ")");
    kvRow(dl, "sent", bytes(net.tx_total) + " (" + rate(net.tx_bps) + ")");
    c.appendChild(dl);
    grid.appendChild(c);

    var rx = (state.history.points || []).map(function (p) { return p.rx_bps; });
    var tx = (state.history.points || []).map(function (p) { return p.tx_bps; });
    var max = Math.max(1, Math.max.apply(null, rx.concat(tx)));
    grid.appendChild(chartCard("Throughput", state.history.range + " window", rx,
      { max: max, legend: ["rx: " + rate(lastOf(rx)), "tx: " + rate(lastOf(tx))] }));

    var ifaces = net.ifaces || [];
    var ifCard = card("Interfaces", ifaces.length + " present");
    if (!ifaces.length) {
      ifCard.appendChild(emptyNote("No interface reported traffic."));
    } else {
      var table = buildTable(["name", "state", "address", "rx", "tx", "errs", "drops"], ["", "num", "", "num", "num", "num", "num"]);
      ifaces.forEach(function (i) {
        addRow(table, [
          i.name,
          i.up ? "up" : "down",
          (i.addrs && i.addrs.length ? i.addrs.join(", ") : i.mac || "-"),
          bytes(i.rx_bps) + "/s",
          bytes(i.tx_bps) + "/s",
          i.rx_errs + i.tx_errs,
          i.rx_drops + i.tx_drops
        ]);
      });
      ifCard.appendChild(table.wrap);
    }
    grid.appendChild(ifCard);

    var listeners = net.listening || [];
    var lCard = card("Listening sockets", listeners.length + " open");
    if (!listeners.length) {
      lCard.appendChild(emptyNote("Nothing is listening."));
    } else {
      var lt = buildTable(["proto", "address", "port", "process", "pid", "reach"], ["", "", "num", "", "num", ""]);
      listeners.forEach(function (l) {
        addRow(lt, [
          l.proto,
          l.addr,
          l.port,
          l.process || "-",
          l.pid || "-",
          l.exposed ? "every interface" : "localhost only"
        ], l.exposed ? ["port.sensitive"] : null);
      });
      lCard.appendChild(lt.wrap);
      if (listeners.some(function (l) { return l.exposed; })) {
        lCard.appendChild(el("div", "card__note", "Sockets on every interface are reachable from the network; those on localhost are not."));
      }
    }
    grid.appendChild(lCard);
  }

  function renderProcesses(snap) {
    var grid = $("processes-grid");
    clear(grid);
    var procs = snap.procs || [];

    var summary = card("Process table", count(snap.total_procs) + " total");
    summary.classList.add("card--span");
    if (!procs.length) {
      summary.appendChild(emptyNote("No process could be read from /proc."));
    } else {
      var t = buildTable(["pid", "user", "cpu %", "mem %", "rss", "threads", "state", "age", "command"],
        ["num", "", "num", "num", "num", "num", "", "num", ""]);
      procs.forEach(function (p) {
        addRow(t, [
          p.pid, p.user || "-", num(p.cpu, 1), num(p.mem_pct, 1), bytes(p.rss),
          p.threads, p.state, duration(p.uptime), p.cmdline || p.name
        ], p.state === "zombie" ? ["warn"] : null, p.cmdline || p.name);
      });
      summary.appendChild(t.wrap);
      summary.appendChild(el("div", "card__note", "CPU percentage is a share of the whole machine, the same convention top uses."));
    }
    grid.appendChild(summary);

    var cpuSeries = (state.history.points || []).map(function (p) { return p.cpu_pct; });
    grid.appendChild(chartCard("Processor history", state.history.range + " window", cpuSeries, { max: 100, threshold: 92, legend: ["last: " + pct(lastOf(cpuSeries), 1)] }));

    var memSeries = (state.history.points || []).map(function (p) { return p.mem_pct; });
    grid.appendChild(chartCard("Memory history", state.history.range + " window", memSeries, { max: 100, threshold: 96, legend: ["last: " + pct(lastOf(memSeries), 1)] }));
  }

  function renderServices(snap) {
    var grid = $("services-grid");
    clear(grid);
    var svc = snap.services || {};
    var sd = svc.systemd || {};
    var dk = svc.docker || {};

    // The version is only shown when there is one: a handful of
    // distributions deny that query to unprivileged users while allowing
    // the unit list, and "v?" in the corner reads like a fault.
    var sdAside = sd.available
      ? ((sd.version ? "v" + sd.version + " \u00B7 " : "") + sd.total + " units")
      : "unavailable";
    var sdCard = card("systemd", sdAside,
      !sd.available ? "" : (sd.failed ? "card--crit" : ""));
    if (!sd.available) {
      sdCard.appendChild(emptyNote(sd.error || "systemd is not reachable."));
    } else {
      var dl = el("dl", "kv");
      kvRow(dl, "units", sd.total, true);
      kvRow(dl, "running", sd.running);
      kvRow(dl, "failed", sd.failed, sd.failed > 0);
      sdCard.appendChild(dl);
      var units = sd.units || [];
      var shown = units.filter(function (u) { return u.status !== "dead" || u.problem; }).slice(0, 60);
      if (!shown.length) {
        sdCard.appendChild(emptyNote("No interesting unit state."));
      } else {
        var t = buildTable(["unit", "state", "sub", "enabled", "restarts"], ["", "", "", "", "num"]);
        shown.forEach(function (u) {
          addRow(t, [u.name, u.active_state, u.sub_state, u.enabled || "-", u.restarts || 0],
            u.status === "failed" ? ["crit"] : (u.problem ? ["warn"] : null));
        });
        sdCard.appendChild(t.wrap);
        if (units.length > shown.length) {
          sdCard.appendChild(el("div", "card__note", (units.length - shown.length) + " inactive unit(s) not shown."));
        }
      }
    }
    grid.appendChild(sdCard);

    var dkAside = dk.available
      ? ((dk.version ? "v" + dk.version + " \u00B7 " : "") + dk.total + " containers")
      : "unavailable";
    var dkCard = card("Docker", dkAside,
      !dk.available ? "" : (dk.unhealthy ? "card--crit" : ""));
    if (!dk.available) {
      dkCard.appendChild(emptyNote(dk.error || "the Docker daemon is not reachable."));
    } else {
      var ddl = el("dl", "kv");
      kvRow(ddl, "containers", dk.total, true);
      kvRow(ddl, "running", dk.running);
      kvRow(ddl, "unhealthy", dk.unhealthy, dk.unhealthy > 0);
      kvRow(ddl, "images", dk.images);
      dkCard.appendChild(ddl);
      var items = dk.items || [];
      if (!items.length) {
        dkCard.appendChild(emptyNote("No containers exist."));
      } else {
        var dt = buildTable(["container", "state", "health", "restarts", "image"], ["", "", "", "num", ""]);
        items.forEach(function (ct) {
          addRow(dt, [ct.name, ct.status, ct.health || "-", ct.restarts || 0, ct.image || "-"],
            ct.status === "restarting" || ct.health === "unhealthy" ? ["crit"] :
              (ct.status === "dead" || ct.status === "paused" ? ["warn"] : null));
        });
        dkCard.appendChild(dt.wrap);
      }
    }
    grid.appendChild(dkCard);

    var svcSeries = (state.history.points || []).map(function (p) { return p.online ? 1 : 0; });
    var online = svcSeries.filter(function (v) { return v === 1; }).length;
    var avail = card("Availability", state.history.range + " window");
    var adl = el("dl", "kv");
    adl = adl;
    kvRow(adl, "samples online", svcSeries.length ? pct(online / svcSeries.length * 100, 1) : "-", true);
    kvRow(adl, "samples recorded", count(svcSeries.length));
    avail.appendChild(adl);
    grid.appendChild(avail);
  }

  function renderRevelations(snap) {
    var grid = $("revelations-grid");
    clear(grid);
    var issues = snap.issues || [];

    var list = card("Findings", issues.length + " reported");
    list.classList.add("card--span");
    if (!issues.length) {
      list.appendChild(emptyNote("Nothing is wrong. The eye stays open anyway."));
    } else {
      var wrap = el("div", "findings");
      issues.forEach(function (i) { wrap.appendChild(findingNode(i)); });
      list.appendChild(wrap);
    }
    grid.appendChild(list);

    var hist = card("Recorded history", state.history.range + " window");
    var hdl = el("dl", "kv");
    var points = state.history.points || [];
    kvRow(hdl, "points", count(points.length), true);
    if (points.length) {
      kvRow(hdl, "from", new Date(points[0].time).toLocaleString());
      kvRow(hdl, "to", new Date(points[points.length - 1].time).toLocaleString());
      var crit = points.reduce(function (a, p) { return a + (p.crit || 0); }, 0);
      kvRow(hdl, "critical moments", count(crit));
    }
    hist.appendChild(hdl);
    if (state.meta && state.meta.history) {
      var h = state.meta.history;
      hist.appendChild(el("div", "card__note",
        "database: " + bytes(h.size) + " \u00B7 raw " + count(h.raw) + " \u00B7 minute " + count(h.minute) + " \u00B7 hour " + count(h.hour)));
    } else {
      hist.appendChild(el("div", "card__note", "history is disabled: no database is configured."));
    }
    grid.appendChild(hist);

    var issueSeries = points.map(function (p) { return p.issues; });
    grid.appendChild(chartCard("Findings over time", state.history.range + " window", issueSeries,
      { max: Math.max(1, Math.max.apply(null, issueSeries.length ? issueSeries : [1])) }));
  }

  /* ---------------------------------------------------------------- table */

  function buildTable(headers, classes) {
    var wrap = el("div", "table-wrap");
    var table = el("table", "data");
    var thead = el("thead");
    var tr = el("tr");
    headers.forEach(function (h, i) {
      var th = el("th", (classes && classes[i]) === "num" ? "num" : null, h);
      tr.appendChild(th);
    });
    thead.appendChild(tr);
    table.appendChild(thead);
    var tbody = el("tbody");
    table.appendChild(tbody);
    wrap.appendChild(table);
    return { wrap: wrap, tbody: tbody, table: table };
  }

  function addRow(t, values, classes, title) {
    var tr = el("tr");
    values.forEach(function (v, i) {
      var cls = [];
      if (t.table.tHead && t.table.tHead.rows[0].cells[i] &&
        t.table.tHead.rows[0].cells[i].classList.contains("num")) cls.push("num");
      if (i === values.length - 1 && typeof v === "string" && v.length > 40) cls.push("trunc");
      var td = el("td", cls.join(" ") || null, v);
      if (classes && classes.indexOf && classes.indexOf("warn") >= 0 && i === 1) td.style.color = "var(--warn)";
      if (classes && classes.indexOf && classes.indexOf("crit") >= 0 && i === 1) td.style.color = "var(--crit-blaze)";
      if (i === 0 && classes) {
        // The first cell carries the status mark for rows that are not OK.
        if (classes.indexOf && classes.indexOf("crit") >= 0) td.insertBefore(mark("crit"), td.firstChild);
        else if (classes.indexOf && classes.indexOf("warn") >= 0) td.insertBefore(mark("warn"), td.firstChild);
      }
      if (title && i === values.length - 1) td.title = title;
      tr.appendChild(td);
    });
    t.tbody.appendChild(tr);
  }

  /* ----------------------------------------------------------------- loop */

  function renderAll() {
    var snap = state.snap;
    if (!snap) return;
    renderMasthead(snap, state.meta);
    renderVerdict(snap);
    renderHardware(snap);
    renderNetwork(snap);
    renderProcesses(snap);
    renderServices(snap);
    renderRevelations(snap);
    renderColophon(snap);
  }

  function renderColophon(snap) {
    $("c-sampled").textContent = "sampled " + ago(state.receivedAt) + " \u00B7 collect " + num(snap.collect_ms, 1) + " ms";
    if (state.meta && state.meta.history) {
      $("c-history").textContent = "history " + bytes(state.meta.history.size);
    } else {
      $("c-history").textContent = "history disabled";
    }
    $("c-version").textContent = "v" + VERSION + (state.session && state.session.demo ? " \u00B7 demo" : "");
  }

  function poll() {
    api("/api/snapshot").then(function (snap) {
      state.snap = snap;
      state.receivedAt = Date.now();
      state.failures = 0;
      renderAll();
    }).catch(function (err) {
      state.failures++;
      if (err.status === 401) {
        showVeil("The session has ended. Enter the passphrase again.");
        return;
      }
      if (state.failures === 3) {
        toast("Cannot reach the panel: " + err.message, "crit");
      }
    });
  }

  function pollHistory() {
    api("/api/history?range=" + encodeURIComponent(state.history.range) + "&points=360")
      .then(function (body) {
        state.history.points = body.points || [];
        renderAll();
      })
      .catch(function () { /* history is a bonus; the live view stands alone */ });
  }

  function pollMeta() {
    api("/api/meta").then(function (meta) {
      state.meta = meta;
      renderColophon(state.snap || { collect_ms: 0 });
      if (meta.history && meta.history.path) {
        // Nothing to say; the colophon already shows the size.
      }
    }).catch(function () { });
  }

  /* ------------------------------------------------------------- interface */

  function toast(message, sev) {
    var t = $("toast");
    t.textContent = message;
    t.dataset.sev = sev || "info";
    t.hidden = false;
    clearTimeout(toast._t);
    toast._t = setTimeout(function () { t.hidden = true; }, 6000);
  }

  function showVeil(message) {
    stopPolling();
    $("app").hidden = true;
    $("veil").hidden = false;
    var err = $("login-error");
    if (message) {
      err.textContent = message;
      err.hidden = false;
    } else {
      err.hidden = true;
    }
    var pw = $("password");
    if (pw) pw.focus();
  }

  function hideVeil() {
    $("veil").hidden = true;
    $("app").hidden = false;
    $("login-error").hidden = true;
  }

  function startPolling() {
    stopPolling();
    poll();
    pollHistory();
    pollMeta();
    state.timer = setInterval(function () {
      poll();
      if (Date.now() - (startPolling._h || 0) > HISTORY_MS) {
        startPolling._h = Date.now();
        pollHistory();
        pollMeta();
      }
    }, POLL_MS);
  }

  function stopPolling() {
    if (state.timer) clearInterval(state.timer);
    state.timer = null;
  }

  function wireRail() {
    var links = document.querySelectorAll(".rail__link");
    links.forEach(function (a) {
      a.addEventListener("click", function (e) {
        e.preventDefault();
        var target = document.querySelector(a.getAttribute("href"));
        if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
      });
    });
    if (!("IntersectionObserver" in window)) return;
    var observer = new IntersectionObserver(function (entries) {
      entries.forEach(function (entry) {
        if (!entry.isIntersecting) return;
        links.forEach(function (a) {
          a.classList.toggle("is-active", a.getAttribute("href") === "#" + entry.target.id);
        });
      });
    }, { rootMargin: "-45% 0px -50% 0px" });
    document.querySelectorAll(".section").forEach(function (s) { observer.observe(s); });
  }

  function wireLogin() {
    var form = $("login-form");
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var button = $("login-button");
      var err = $("login-error");
      button.disabled = true;
      button.textContent = "OPENING";
      err.hidden = true;
      api("/api/auth/login", { method: "POST", body: { password: $("password").value } })
        .then(function () {
          $("password").value = "";
          hideVeil();
          startPolling();
        })
        .catch(function (error) {
          err.textContent = error.status === 429
            ? "Too many attempts. Wait " + ((error.body && error.body.retry_in_seconds) || 60) + " seconds."
            : "That is not the passphrase.";
          err.hidden = false;
          $("password").select();
        })
        .finally(function () {
          button.disabled = false;
          button.textContent = "OPEN THE EYE";
        });
    });
  }

  function wireLogout() {
    $("logout").addEventListener("click", function () {
      api("/api/auth/logout", { method: "POST" }).finally(function () {
        state.snap = null;
        showVeil("The eye is closed.");
      });
    });
  }

  var _ = 0;
  void _;

  function boot() {
    wireLogin();
    wireLogout();
    wireRail();
    api("/api/auth/session").then(function (session) {
      state.session = session;
      if (session.poll_ms) POLL_MS = session.poll_ms;
      if (session.authenticated) {
        hideVeil();
        startPolling();
      } else {
        showVeil(session.demo ? "Demo mode: no passphrase is required. Press OPEN THE EYE." : "");
      }
    }).catch(function () {
      showVeil("");
    });
  }

  document.addEventListener("visibilitychange", function () {
    // Stop asking while nobody is looking, resume immediately when the
    // tab comes back: a panel left open overnight should not hammer the
    // machine for nothing.
    if (document.hidden) {
      stopPolling();
    } else if (state.session && state.session.authenticated && !$("app").hidden) {
      startPolling();
    }
  });

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
