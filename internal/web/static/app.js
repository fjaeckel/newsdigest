(function () {
  "use strict";

  var date = document.body.dataset.date;
  var unreadEl = document.getElementById("unread-count");

  function post(url, payload) {
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    }).then(function (r) {
      if (!r.ok) throw new Error(r.statusText);
      return r.json();
    });
  }

  function setUnread(n) {
    if (unreadEl && typeof n === "number") unreadEl.textContent = n;
  }

  // --- mark one topic read/unread ---

  document.querySelectorAll(".topic").forEach(function (topic) {
    var button = topic.querySelector(".topic-main");
    if (!button) return;

    button.addEventListener("click", function () {
      var read = !topic.classList.contains("is-read");
      topic.classList.toggle("is-read", read); // optimistic

      // A tag feed mixes days on one page, so each card carries its own date;
      // the digest page falls back to the single date on <body>.
      var topicDate = topic.dataset.date || date;

      post("/api/read", { date: topicDate, id: topic.dataset.id, read: read })
        .then(function (res) { setUnread(res.unread); })
        .catch(function () { topic.classList.toggle("is-read", !read); });
    });
  });

  // --- mark all read ---

  var markAll = document.getElementById("mark-all");
  if (markAll) {
    markAll.addEventListener("click", function () {
      var topics = document.querySelectorAll(".topic");
      var allRead = Array.prototype.every.call(topics, function (t) {
        return t.classList.contains("is-read");
      });
      var read = !allRead;

      topics.forEach(function (t) { t.classList.toggle("is-read", read); });
      markAll.textContent = read ? "Unmark all" : "Mark all";

      post("/api/read-all", { date: date, read: read })
        .then(function (res) { setUnread(res.unread); })
        .catch(function () {
          topics.forEach(function (t) { t.classList.toggle("is-read", !read); });
        });
    });
  }

  // --- hide-read toggle, remembered per device ---
  // A category feed is a standing inbox: it carries unread topics forward
  // indefinitely, so read ones drop out by default and the toggle brings them
  // back. The daily brief is a snapshot of one day and defaults the other way,
  // so the two pages keep their preference under separate keys.

  var isFeed = document.body.classList.contains("cat-feed");
  var readKey = isFeed ? "hideReadFeed" : "hideRead";

  var toggle = document.getElementById("toggle-read");
  if (toggle) {
    var stored = localStorage.getItem(readKey);
    var hidden = stored === null ? isFeed : stored === "1";
    apply(hidden);

    toggle.addEventListener("click", function () {
      hidden = !hidden;
      localStorage.setItem(readKey, hidden ? "1" : "0");
      apply(hidden);
    });
  }

  function apply(on) {
    document.body.classList.toggle("hide-read", on);
    toggle.setAttribute("aria-pressed", on ? "true" : "false");
    toggle.textContent = on ? "Show read" : "Hide read";
  }

  // --- clear a whole standing feed ---
  // The feed spans every day it has ever briefed, so this can't be done by
  // walking the page; the server marks the category across all of them.

  var markCat = document.getElementById("mark-category");
  if (markCat) {
    markCat.addEventListener("click", function () {
      var topics = document.querySelectorAll(".topic");
      var allRead = Array.prototype.every.call(topics, function (t) {
        return t.classList.contains("is-read");
      });
      var read = !allRead;

      topics.forEach(function (t) { t.classList.toggle("is-read", read); });
      markCat.textContent = read ? "Unmark all" : "Mark all";

      post("/api/read-category", { category: document.body.dataset.category, read: read })
        .then(function (res) { setUnread(res.unread); })
        .catch(function () {
          topics.forEach(function (t) { t.classList.toggle("is-read", !read); });
        });
    });
  }

  // --- whether outlets with several articles fan out by default ---
  // Off keeps the brief dense; either way any outlet can still be opened by
  // tapping it. Remembered per device.

  var srcToggle = document.getElementById("toggle-sources");
  if (srcToggle) {
    var groups = document.querySelectorAll(".src-group details");

    if (!groups.length) {
      // Nothing to fan out today - don't offer a control that does nothing.
      srcToggle.remove();
    } else {
      var fanned = localStorage.getItem("expandSources") === "1";
      applySources(fanned);

      srcToggle.addEventListener("click", function () {
        fanned = !fanned;
        localStorage.setItem("expandSources", fanned ? "1" : "0");
        applySources(fanned);
      });
    }
  }

  function applySources(on) {
    document.querySelectorAll(".src-group details").forEach(function (d) {
      d.open = on;
    });
    srcToggle.setAttribute("aria-pressed", on ? "true" : "false");
    srcToggle.textContent = on ? "Collapse sources" : "Expand sources";
  }

  // --- manual refresh: kick off a run, then poll until it lands ---

  var refresh = document.getElementById("refresh");
  if (refresh) {
    refresh.addEventListener("click", function () {
      refresh.disabled = true;
      refresh.textContent = "Refreshing…";

      post("/api/refresh", {})
        .then(function () { setTimeout(poll, 5000); })
        .catch(function () {
          refresh.disabled = false;
          refresh.textContent = "Refresh";
        });
    });
  }

  var polls = 0;
  function poll() {
    // Give up after ~10 minutes and let the user reload by hand.
    if (++polls > 120) {
      refresh.disabled = false;
      refresh.textContent = "Refresh";
      return;
    }
    fetch("/api/status")
      .then(function (r) { return r.json(); })
      .then(function (s) {
        if (s.generating) {
          setTimeout(poll, 5000);
          return;
        }
        location.reload();
      })
      .catch(function () { setTimeout(poll, 5000); });
  }
})();
