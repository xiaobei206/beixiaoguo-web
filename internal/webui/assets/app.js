(function () {
  "use strict";

  const $ = (selector) => document.querySelector(selector);
  const state = {
    view: "discover",
    cursor: "",
    hasMore: false,
    busy: false,
    items: [],
    favorites: loadFavorites(),
    toastTimer: 0
  };

  const navLabels = {
    discover: ["为你精选", "发现好剧", "从这里开始，找到下一部喜欢的故事。"],
    rank: ["人气风向", "排行榜", "看看大家最近都在追什么。"],
    new: ["每日上新", "新剧上线", "最近更新的短剧与新鲜故事。"],
    hot: ["正在热议", "热搜榜", "看看当下大家都在搜什么。"],
    mine: ["留住喜欢", "我的收藏", "收藏的故事都在这里。"],
    search: ["搜索结果", "找到这些", "换个关键词，发现更多好故事。"]
  };

  function escapeHTML(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (ch) {
      return {"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[ch];
    });
  }

  function safeImage(value) {
    const url = String(value || "");
    return /^https:\/\//i.test(url) ? url : "";
  }

  function loadFavorites() {
    try {
      const parsed = JSON.parse(localStorage.getItem("beixiaoguo.favorites") || "[]");
      return Array.isArray(parsed) ? parsed : [];
    } catch (_) {
      return [];
    }
  }

  function saveFavorites() {
    try {
      localStorage.setItem("beixiaoguo.favorites", JSON.stringify(state.favorites));
    } catch (_) {
      showToast("浏览器存储空间不可用，收藏未能保存");
    }
  }

  function showToast(message) {
    const el = $("#toast");
    el.textContent = message;
    el.classList.add("show");
    window.clearTimeout(state.toastTimer);
    state.toastTimer = window.setTimeout(() => el.classList.remove("show"), 2400);
  }

  async function request(path) {
    const response = await fetch(path, {headers: {"Accept":"application/json"}});
    const payload = await response.json();
    if (!response.ok) {
      const err = payload && payload.error ? payload.error : {};
      throw new Error(err.message || ("HTTP " + response.status));
    }
    return payload;
  }

  function normalize(item) {
    return {
      id: item.id || item.series_id || item.seriesId || "",
      title: item.title || "未命名短剧",
      cover_url: item.cover_url || item.coverUrl || "",
      description: item.description || "",
      episode_count: item.episode_count || item.episodeCount || 0,
      heat: item.heat || item.search_score || item.score || "",
      tag: item.tag || "",
      raw: item
    };
  }

  function setHeading(view) {
    const labels = navLabels[view] || navLabels.discover;
    $("#section-kicker").textContent = labels[0];
    $("#section-title").textContent = labels[1];
    $("#section-subtitle").textContent = labels[2];
    document.querySelectorAll(".nav-item").forEach((button) => {
      button.classList.toggle("active", button.dataset.view === view);
    });
    $("#welcome").hidden = view !== "discover";
  }

  function renderCards(items) {
    const grid = $("#series-grid");
    if (!items.length) {
      grid.innerHTML = '<div class="empty-state">这里暂时没有内容，试试刷新或换个关键词。</div>';
      return;
    }
    grid.innerHTML = items.map((item) => {
      const data = normalize(item);
      const cover = safeImage(data.cover_url);
      const fav = state.favorites.some((entry) => entry.id === data.id);
      const tag = data.tag ? '<span class="poster-tag">' + escapeHTML(data.tag) + '</span>' : "";
      return '<article class="series-card" tabindex="0" role="button" data-id="' + escapeHTML(data.id) + '" aria-label="查看 ' + escapeHTML(data.title) + '">' +
        '<div class="poster-wrap">' +
        (cover ? '<img loading="lazy" src="' + escapeHTML(cover) + '" alt="' + escapeHTML(data.title) + '">' : '<div class="poster-placeholder">贝</div>') +
        tag + (fav ? '<span class="poster-tag favorite-tag">♥</span>' : "") +
        '</div><h3 class="card-title">' + escapeHTML(data.title) + '</h3>' +
        '<div class="card-meta">' + escapeHTML(data.episode_count ? data.episode_count + " 集" : data.heat || "短剧") + '</div>' +
        '</article>';
    }).join("");
  }

  function renderHero(items) {
    const el = $("#hero");
    const first = items.length ? normalize(items[0]) : null;
    if (!first || !safeImage(first.cover_url)) {
      el.hidden = true;
      return;
    }
    el.hidden = false;
    el.innerHTML = '<div class="feature-inner"><img loading="lazy" src="' + escapeHTML(safeImage(first.cover_url)) + '" alt="">' +
      '<div class="feature-text"><div class="feature-label">TODAY’S PICK</div><h3>' + escapeHTML(first.title) + '</h3>' +
      '<p>' + escapeHTML(first.description || (first.episode_count ? first.episode_count + " 集 · 点开查看详情" : "点开查看剧集详情")) + '</p></div></div>';
  }

  function setStatus(message, isError) {
    const el = $("#status");
    el.textContent = message || "";
    el.classList.toggle("error", !!isError);
  }

  function setBusy(busy) {
    state.busy = busy;
    $("#refresh-button").disabled = busy;
    $("#load-more").disabled = busy;
    $("#load-more").textContent = busy ? "正在加载…" : "加载更多";
  }

  async function loadView(view, append) {
    if (state.busy) return;
    if (!append) {
      state.view = view;
      state.cursor = "";
      state.hasMore = false;
      state.items = [];
      setHeading(view);
      $("#hero").hidden = true;
      $("#series-grid").innerHTML = "";
    }
    if (view === "mine") {
      renderCards(state.favorites);
      $("#load-more").hidden = true;
      setStatus(state.favorites.length ? "收藏保存在当前浏览器，不会上传到 N1。" : "还没有收藏的短剧。打开剧集详情即可收藏。");
      return;
    }
    setBusy(true);
    setStatus(append ? "正在加载下一页…" : "正在连接片库…");
    try {
      let data;
      let items = [];
      let endpoint = "";
      if (view === "discover") {
        endpoint = "/api/v1/discover" + (append && state.cursor ? "?cursor=" + encodeURIComponent(state.cursor) : "");
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "search") {
        const q = $("#search-input").value.trim();
        endpoint = "/api/v1/search?q=" + encodeURIComponent(q) + (append && state.cursor ? "&cursor=" + encodeURIComponent(state.cursor) : "");
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "new") {
        endpoint = "/api/v1/new-releases";
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "rank") {
        endpoint = "/api/v1/rank" + (append && state.cursor ? "?cursor=" + encodeURIComponent(state.cursor) : "");
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "hot") {
        data = await request("/api/v1/hot-search");
        const boards = (data.boards || []).flatMap((board) => board.items || []);
        items = (data.guesses || []).concat(boards);
      }
      const mapped = items.map(normalize).filter((item) => item.id);
      state.items = append ? state.items.concat(mapped) : mapped;
      state.cursor = data.cursor || "";
      state.hasMore = !!data.has_more && !!state.cursor;
      renderCards(state.items);
      if (!append && view === "discover") renderHero(state.items);
      $("#load-more").hidden = !state.hasMore;
      setStatus(state.items.length ? ("已载入 " + state.items.length + " 部") : "暂无内容");
    } catch (error) {
      setStatus(error.message || "加载失败", true);
      if (!append) $("#series-grid").innerHTML = '<div class="empty-state">片库暂时没有响应。<br>请检查 N1 网络后点击刷新。</div>';
    } finally {
      setBusy(false);
    }
  }

  function stopPlayback() {
    const video = $("#web-player");
    if (!video) return;
    video.pause();
    video.removeAttribute("src");
    video.load();
  }

  async function openDetail(id) {
    if (!id) return;
    stopPlayback();
    const dialog = $("#detail-dialog");
    const body = $("#detail-content");
    body.innerHTML = '<div class="empty-state">正在载入剧集详情…</div>';
    if (!dialog.open) dialog.showModal();
    try {
      const [series, episodePage] = await Promise.all([
        request("/api/v1/series/" + encodeURIComponent(id)),
        request("/api/v1/series/" + encodeURIComponent(id) + "/episodes")
      ]);
      const episodes = episodePage.items || [];
      const cover = safeImage(series.cover_url || series.coverUrl);
      const fav = state.favorites.some((entry) => entry.id === id);
      body.innerHTML =
        '<div class="detail-hero">' +
        (cover ? '<img src="' + escapeHTML(cover) + '" alt="">' : '<div class="poster-placeholder detail-placeholder">贝</div>') +
        '<div class="detail-copy"><div class="detail-meta">' + escapeHTML(series.right_text || series.subtitles || "贝小果片库") + '</div>' +
        '<h2>' + escapeHTML(series.title || "短剧详情") + '</h2>' +
        '<div class="detail-meta">' + escapeHTML(episodes.length + " 集") + '</div>' +
        '<p>' + escapeHTML(series.description || "暂无简介") + '</p>' +
        '<button id="favorite-toggle" class="load-more" type="button">' + (fav ? "♥ 已收藏" : "♡ 收藏这部剧") + '</button></div></div>' +
        '<section id="playback-panel" class="playback-panel" hidden>' +
        '<div class="player-toolbar"><span id="player-caption">正在准备播放…</span>' +
        '<label class="quality-control">画质 <select id="player-quality" aria-label="播放画质"><option value="best">自动</option></select></label>' +
        '<button id="player-close" class="player-close" type="button">收起</button></div>' +
        '<video id="web-player" controls playsinline webkit-playsinline preload="metadata"></video>' +
        '<div id="playback-status" class="playback-status" role="status">选择集数开始播放</div></section>' +
        '<h3 class="episode-heading">选集</h3><div class="episode-grid">' +
        episodes.map((ep) => '<button class="episode-chip" type="button" data-episode-id="' + escapeHTML(ep.id) + '" data-episode-number="' + escapeHTML(ep.number || "") + '"' + (ep.playable === false ? ' disabled title="当前分集不可播放"' : '') + '>' + escapeHTML("第 " + (ep.number || "") + " 集") + '</button>').join("") +
        '</div><div class="player-note">视频由 N1 按 HTTP Range 按需读取与解密，不下载整集、不转码。画中画、横屏和硬件解码能力由当前浏览器与设备决定。</div>';

      $("#favorite-toggle").addEventListener("click", () => toggleFavorite(normalize(series)));
      body.querySelector(".episode-grid").addEventListener("click", (event) => {
        const button = event.target.closest(".episode-chip");
        if (!button || button.disabled) return;
        startPlayback(button.dataset.episodeId, button.dataset.episodeNumber, series.title || "贝小果短剧");
      });
      $("#player-close").addEventListener("click", () => {
        const video = $("#web-player");
        video.pause();
        video.removeAttribute("src");
        video.load();
        $("#playback-panel").hidden = true;
      });
    } catch (error) {
      body.innerHTML = '<div class="empty-state">' + escapeHTML(error.message || "详情载入失败") + '</div>';
    }
  }

  async function startPlayback(episodeId, episodeNumber, seriesTitle, quality, resumeAt) {
    const panel = $("#playback-panel");
    const video = $("#web-player");
    const status = $("#playback-status");
    const caption = $("#player-caption");
    const qualitySelect = $("#player-quality");
    panel.hidden = false;
    caption.textContent = (seriesTitle || "短剧") + " · 第 " + (episodeNumber || "?") + " 集";
    status.textContent = "正在解析播放地址…";
    video.pause();
    video.removeAttribute("src");
    video.load();

    try {
      const chosen = quality || "best";
      const info = await request("/api/v1/play/" + encodeURIComponent(episodeId) + "?quality=" + encodeURIComponent(chosen));
      qualitySelect.innerHTML = (info.available_qualities || []).map((item) =>
        '<option value="' + escapeHTML(item.label) + '">' + escapeHTML(item.label) + (item.codec ? " · " + escapeHTML(item.codec) : "") + '</option>'
      ).join("");
      if (!qualitySelect.options.length) {
        qualitySelect.innerHTML = '<option value="best">自动</option>';
      }
      qualitySelect.value = info.quality || chosen;
      qualitySelect.onchange = () => {
        const position = Number.isFinite(video.currentTime) ? video.currentTime : 0;
        startPlayback(episodeId, episodeNumber, seriesTitle, qualitySelect.value, position);
      };

      status.textContent = "已取得 " + (info.quality || "自动") + " 播放流" +
        (info.protected ? " · N1 正在按需解密" : " · 明文 MP4") +
        "。正在连接视频…";
      const target = Math.max(0, Number(resumeAt) || 0);
      if (target > 0) {
        video.addEventListener("loadedmetadata", () => {
          try { video.currentTime = Math.min(target, Math.max(0, video.duration - 1)); } catch (_) {}
        }, {once:true});
      }
      video.onerror = () => {
        const code = video.error ? video.error.code : 0;
        status.textContent = "浏览器无法播放此视频（媒体错误 " + code + "）。可尝试切换画质或使用其他浏览器。";
      };
      video.src = info.src;
      video.load();
      try {
        await video.play();
        status.textContent = "正在播放 · " + (info.quality || "自动") +
          (document.pictureInPictureEnabled ? " · 支持画中画" : "");
      } catch (_) {
        status.textContent = "播放地址已就绪，点击视频播放键开始。";
      }
    } catch (error) {
      status.textContent = error.message || "播放初始化失败";
      showToast(error.message || "播放初始化失败");
    }
  }

  function toggleFavorite(item) {
    const index = state.favorites.findIndex((entry) => entry.id === item.id);
    if (index >= 0) {
      state.favorites.splice(index, 1);
      showToast("已从收藏移除");
    } else {
      state.favorites.unshift(item);
      showToast("已加入收藏");
    }
    saveFavorites();
    if ($("#detail-dialog").open) {
      const button = $("#favorite-toggle");
      if (button) button.textContent = state.favorites.some((entry) => entry.id === item.id) ? "♥ 已收藏" : "♡ 收藏这部剧";
    }
    if (state.view === "mine") renderCards(state.favorites);
  }

  document.querySelectorAll(".nav-item").forEach((button) => {
    button.addEventListener("click", () => loadView(button.dataset.view, false));
  });
  $("#search-form").addEventListener("submit", (event) => {
    event.preventDefault();
    if (!$("#search-input").value.trim()) return;
    loadView("search", false);
  });
  $("#refresh-button").addEventListener("click", () => loadView(state.view, false));
  $("#load-more").addEventListener("click", () => loadView(state.view, true));
  $("#series-grid").addEventListener("click", (event) => {
    const card = event.target.closest(".series-card");
    if (card) openDetail(card.dataset.id);
  });
  $("#series-grid").addEventListener("keydown", (event) => {
    if (event.key !== "Enter" && event.key !== " ") return;
    const card = event.target.closest(".series-card");
    if (card) {
      event.preventDefault();
      openDetail(card.dataset.id);
    }
  });
  $("#dialog-close").addEventListener("click", () => $("#detail-dialog").close());
  $("#detail-dialog").addEventListener("close", stopPlayback);
  $("#detail-dialog").addEventListener("click", (event) => {
    if (event.target === $("#detail-dialog")) $("#detail-dialog").close();
  });

  loadView("discover", false);
})();
