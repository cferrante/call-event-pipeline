const $ = id => document.getElementById(id);
const cell = (text, cls) => { const td = document.createElement("td"); td.textContent = text; if (cls) td.className = cls; return td; };
const row = cells => { const tr = document.createElement("tr"); cells.forEach(c => tr.appendChild(c)); return tr; };
const fmtAge = s => s <= 0 ? "–" : s < 120 ? s.toFixed(0) + " s" : (s / 60).toFixed(1) + " min";
const time = t => new Date(t).toLocaleTimeString();

async function refresh() {
  try {
    const r = await fetch("/status", { cache: "no-store" });
    const st = await r.json();
    const banner = $("banner");
    if (st.SampleError) { banner.className = "banner bad"; banner.textContent = "Can't read the pipeline: " + st.SampleError; }
    else if (st.Active.length) { banner.className = "banner bad"; banner.textContent = st.Active.length + " alert(s) firing"; }
    else { banner.className = "banner ok"; banner.textContent = "Healthy: no alerts"; }
    $("arrivals").textContent = st.ArrivalsPerSec.toFixed(1) + " / s";
    $("at").textContent = st.At.startsWith("0001") ? "–" : time(st.At);

    const names = Object.keys(st.Consumers).sort();
    $("consumers").replaceChildren(...(names.length ? names.map(n => {
      const c = st.Consumers[n];
      return row([cell(n), cell(c.Lag.toLocaleString()), cell(fmtAge(c.OldestPendingSeconds), c.OldestPendingSeconds > 120 ? "stale" : ""),
                  cell(c.WrittenPerSec.toFixed(1))]);
    }) : [row([cell("None yet", "muted")])]));

    const lines = [
      ...st.Active.map(a => row([cell("firing", "firing"), cell(a.Rule + (a.Subject ? " (" + a.Subject + ")" : "")), cell(a.Detail), cell("since " + time(a.Since))])),
      ...st.Recent.filter(e => e.State === "resolved").map(e => row([cell("resolved", "resolved"), cell(e.Rule + (e.Subject ? " (" + e.Subject + ")" : "")),
        cell(e.Detail), cell(time(e.At) + " after " + fmtAge(e.DurationSeconds))])),
    ];
    $("alerts").replaceChildren(...(lines.length ? lines : [row([cell("No alerts", "muted")])]));

    const sent = st.Outbox.map(d => row([cell(d.State, d.State === "sent" ? "resolved" : d.State === "retrying" ? "stale" : d.State === "queued" ? "" : "firing"),
      cell(d.Event + ": " + d.Rule + (d.Subject ? " (" + d.Subject + ")" : "")), cell(String(d.Attempts)), cell(d.LastError || "–"), cell(time(d.Updated))]));
    $("outbox").replaceChildren(...(sent.length ? sent : [row([cell(st.EmailOn ? "No alert emails yet" : "Email alerts are off (set ALERT_EMAIL_TO)", "muted")])]));
  } catch (err) {
    $("banner").className = "banner bad";
    $("banner").textContent = "The monitor isn't answering: " + err.message;
  }
}
refresh();
setInterval(refresh, 2000);
