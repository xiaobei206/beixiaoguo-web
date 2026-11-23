(function () {
  "use strict";

  const $ = (selector) => document.querySelector(selector);
  function todayDateKey() {
    const now = new Date();
    return String(now.getFullYear()) + String(now.getMonth() + 1).padStart(2, "0") + String(now.getDate()).padStart(2, "0");
  }

  const state = {
    view: "discover",
    cursor: "",
    hasMore: false,
    busy: false,
    items: [],
    pageCount: 0,
    discoverChannel: "推荐",
    searchType: "all",
    rankContent: "all",
    rankBoard: "",
    rankContents: [],
    newDate: todayDateKey(),
    newDates: [],
    favorites: loadFavorites(),
    toastTimer: 0,
    viewCache: Object.create(null),
    activeViewKey: "",
    loadSequence: 0,
    autoLoadScheduled: false,
    playback: {seriesId:"",seriesTitle:"",episodes:[],index:-1,quality:"best",speed:1,requestId:0}
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
    return /^\/api\/v1\/images\/[a-f0-9]{32}$/i.test(url) ? url : "";
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

  async function request(path, options) {
    options = options || {};
    const headers = Object.assign({"Accept":"application/json"}, options.headers || {});
    const response = await fetch(path, Object.assign({}, options, {headers}));
    const payload = await response.json();
    if (!response.ok) {
      const err = payload && payload.error ? payload.error : {};
      throw new Error(err.message || ("HTTP " + response.status));
    }
    return payload;
  }

  function normalize(item) {
    return {
      id: item.id || item.series_id || item.seriesId || item.actor_id || item.actorId || "",
      title: item.title || item.name || "未命名短剧",
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

  function renderCards(items, append) {
    const grid = $("#series-grid");
    if (!items.length) {
      if (append && grid.querySelector(".series-card")) return;
      grid.innerHTML = '<div class="empty-state">这里暂时没有内容，试试刷新或换个关键词。</div>';
      return;
    }
    const markup = items.map((item) => {
      const data = normalize(item);
      const cover = safeImage(data.cover_url);
      const fav = state.favorites.some((entry) => entry.id === data.id);
      const tag = data.tag ? '<span class="poster-tag">' + escapeHTML(data.tag) + '</span>' : "";
      return '<article class="series-card" tabindex="0" role="button" data-id="' + escapeHTML(data.id) + '" aria-label="查看 ' + escapeHTML(data.title) + '">' +
        '<div class="poster-wrap">' +
        (cover ? '<img loading="lazy" decoding="async" src="' + escapeHTML(cover) + '" alt="' + escapeHTML(data.title) + '">' : '<div class="poster-placeholder">贝</div>') +
        tag + (fav ? '<span class="poster-tag favorite-tag">♥</span>' : "") +
        '</div><h3 class="card-title">' + escapeHTML(data.title) + '</h3>' +
        '<div class="card-meta">' + escapeHTML(data.episode_count ? data.episode_count + " 集" : data.heat || "短剧") + '</div>' +
        '</article>';
    }).join("");
    if (append) {
      const empty = grid.querySelector(".empty-state");
      if (empty) empty.remove();
      grid.insertAdjacentHTML("beforeend", markup);
    } else {
      grid.innerHTML = markup;
    }
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
    renderPagination();
  }

  const discoverChannels = [
    ["推荐", "推荐"], ["真人剧", "真人剧"], ["漫剧", "漫剧"],
    ["找剧", "找剧"], ["电影", "电影"], ["小说", "小说"]
  ];
  const searchTypes = [
    ["all", "全部"], ["short", "真人剧"], ["comic", "漫剧"],
    ["ai", "AI漫剧"], ["film", "影视剧"], ["unwatched", "未看过"]
  ];

  function tagRow(label, kind, options) {
    if (!options || !options.length) return "";
    return '<div class="filter-row"><span class="filter-label">' + escapeHTML(label) + '</span><div class="filter-list">' +
      options.map((option) => '<button type="button" class="filter-tag' + (option.active ? " active" : "") +
        '" data-filter-kind="' + escapeHTML(kind) + '" data-filter-value="' + escapeHTML(option.value) +
        '" aria-pressed="' + (option.active ? "true" : "false") + '">' + escapeHTML(option.label) + '</button>').join("") +
      '</div></div>';
  }

  function formatDateTag(value) {
    const raw = String(value || "");
    const match = raw.match(/^(\d{4})-?(\d{2})-?(\d{2})$/);
    if (!match) return raw;
    const year = Number(match[1]), month = Number(match[2]), day = Number(match[3]);
    const dayValue = Date.UTC(year, month - 1, day);
    const now = new Date();
    const todayValue = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate());
    const delta = Math.round((dayValue - todayValue) / 86400000);
    if (delta === 0) return "今天";
    if (delta === -1) return "昨天";
    if (delta === 1) return "明天";
    return month + "/" + day;
  }

  function renderFilterTags() {
    const container = $("#filter-tags");
    if (!container) return;
    const rows = [];
    if (state.view === "discover") {
      rows.push(tagRow("频道", "channel", discoverChannels.map(([value, label]) => ({
        value, label, active: state.discoverChannel === value
      }))));
    } else if (state.view === "search") {
      rows.push(tagRow("类型", "search-type", searchTypes.map(([value, label]) => ({
        value, label, active: state.searchType === value
      }))));
    } else if (state.view === "rank" && state.rankContents.length) {
      rows.push(tagRow("类型", "rank-content", state.rankContents.map((content) => ({
        value: content.id, label: content.name, active: content.id === state.rankContent
      }))));
      const selectedContent = state.rankContents.find((item) => item.id === state.rankContent) ||
        state.rankContents.find((item) => item.selected) || state.rankContents[0];
      if (selectedContent && Array.isArray(selectedContent.boards) && selectedContent.boards.length) {
        rows.push(tagRow("榜单", "rank-board", selectedContent.boards.map((board) => ({
          value: board.id, label: board.name, active: board.id === state.rankBoard
        }))));
      }
    } else if (state.view === "new" && state.newDates.length) {
      rows.push(tagRow("日期", "new-date", state.newDates.map((value) => ({
        value, label: formatDateTag(value), active: String(value) === String(state.newDate)
      }))));
    }
    container.innerHTML = rows.join("");
    container.hidden = rows.length === 0;
  }

  function renderPagination() {
    const info = $("#pagination-tag");
    const more = $("#load-more");
    if (!info || !more) return;
    const paged = ["discover", "search", "rank", "new"].includes(state.view);
    if (!paged || !state.items.length) {
      info.hidden = true;
      more.hidden = true;
      return;
    }
    let label = "已载入 " + state.items.length + " 部";
    if (state.pageCount > 0) label += " · 第 " + state.pageCount + " 页";
    if (state.busy) label += " · 加载中";
    else if (state.hasMore) label += " · 向下滚动自动加载";
    else label += " · 已加载完毕";
    info.textContent = label;
    info.hidden = false;
    more.hidden = !state.hasMore;
  }

  function viewCacheKey(view) {
    if (view === "search") return "search:" + $("#search-input").value.trim() + ":" + state.searchType;
    if (view === "discover") return "discover:" + state.discoverChannel;
    if (view === "rank") return "rank:" + (state.rankContent || "all") + ":" + (state.rankBoard || "default");
    if (view === "new") return "new:" + (state.newDate || todayDateKey());
    return view;
  }

  function saveActiveView() {
    if (!state.activeViewKey || state.view === "mine" || !state.items.length) return;
    state.viewCache[state.activeViewKey] = {
      items: state.items.slice(),
      cursor: state.cursor,
      hasMore: state.hasMore,
      pageCount: state.pageCount,
      discoverChannel: state.discoverChannel,
      searchType: state.searchType,
      rankContent: state.rankContent,
      rankBoard: state.rankBoard,
      rankContents: state.rankContents.slice(),
      newDate: state.newDate,
      newDates: state.newDates.slice(),
      scrollY: window.scrollY,
      savedAt: Date.now()
    };
    const keys = Object.keys(state.viewCache);
    if (keys.length > 12) {
      keys.sort((a, b) => state.viewCache[a].savedAt - state.viewCache[b].savedAt);
      delete state.viewCache[keys[0]];
    }
  }

  function restoreView(view, key, cached) {
    state.view = view;
    state.activeViewKey = key;
    state.items = cached.items.slice();
    state.cursor = cached.cursor;
    state.hasMore = cached.hasMore;
    state.pageCount = cached.pageCount || (cached.items.length ? 1 : 0);
    state.discoverChannel = cached.discoverChannel || state.discoverChannel;
    state.searchType = cached.searchType || "all";
    state.rankContent = cached.rankContent || "all";
    state.rankBoard = cached.rankBoard || "";
    state.rankContents = Array.isArray(cached.rankContents) ? cached.rankContents.slice() : [];
    state.newDate = cached.newDate || state.newDate || todayDateKey();
    state.newDates = Array.isArray(cached.newDates) ? cached.newDates.slice() : [];
    setHeading(view);
    renderFilterTags();
    renderCards(state.items);
    if (view === "discover") renderHero(state.items);
    else $("#hero").hidden = true;
    renderPagination();
    setStatus("已从页面缓存恢复 · " + state.items.length + " 部");
    window.requestAnimationFrame(() => window.scrollTo({top:cached.scrollY || 0,behavior:"auto"}));
  }

  async function loadView(view, append, force) {
    if (append && state.busy) return;
    if (!append) {
      const key = viewCacheKey(view);
      if (state.activeViewKey && state.activeViewKey !== key && state.view !== view) saveActiveView();
      state.loadSequence++;
      state.busy = false;
      setBusy(false);
      state.view = view;
      state.activeViewKey = key;
      setHeading(view);
      renderFilterTags();

      if (view === "mine") {
        state.cursor = "";
        state.hasMore = false;
        state.pageCount = 0;
        state.items = [];
        renderCards(state.favorites);
        $("#hero").hidden = true;
        renderFilterTags();
        renderPagination();
        setStatus(state.favorites.length ? "收藏保存在当前浏览器，不会上传到 N1。" : "还没有收藏的短剧。打开剧集详情即可收藏。");
        return;
      }

      const cached = state.viewCache[key];
      if (!force && cached) {
        restoreView(view, key, cached);
        return;
      }
      state.cursor = "";
      state.hasMore = false;
      state.pageCount = 0;
      state.items = [];
      $("#hero").hidden = true;
      $("#series-grid").innerHTML = "";
      renderPagination();
    }

    const loadID = ++state.loadSequence;
    const activeKey = state.activeViewKey;
    setBusy(true);
    setStatus(append ? "正在加载下一页…" : "正在连接片库…");
    let loadSucceeded = false;
    try {
      let data;
      let items = [];
      let endpoint = "";
      if (view === "discover") {
        const params = new URLSearchParams();
        params.set("channel", state.discoverChannel || "推荐");
        if (append && state.cursor) params.set("cursor", state.cursor);
        endpoint = "/api/v1/discover?" + params.toString();
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "search") {
        const params = new URLSearchParams();
        params.set("q", $("#search-input").value.trim());
        if (append && state.cursor) params.set("cursor", state.cursor);
        if (state.searchType && state.searchType !== "all") params.set("type", state.searchType);
        endpoint = "/api/v1/search?" + params.toString();
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "new") {
        const params = new URLSearchParams();
        params.set("date", state.newDate || todayDateKey());
        if (append && state.cursor) params.set("cursor", state.cursor);
        endpoint = "/api/v1/new-releases?" + params.toString();
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "rank") {
        const params = new URLSearchParams();
        if (state.rankContent && state.rankContent !== "all") params.set("content", state.rankContent);
        if (state.rankBoard) params.set("board", state.rankBoard);
        if (append && state.cursor) params.set("cursor", state.cursor);
        endpoint = "/api/v1/rank" + (params.toString() ? "?" + params.toString() : "");
        data = await request(endpoint);
        items = data.items || [];
      } else if (view === "hot") {
        data = await request("/api/v1/hot-search");
        const boards = (data.boards || []).flatMap((board) => board.items || []);
        items = (data.guesses || []).concat(boards);
      }
      if (loadID !== state.loadSequence || activeKey !== state.activeViewKey) return;

      const mapped = items.map(normalize).filter((item) => item.id);
      const previousCount = append ? state.items.length : 0;
      const combined = append ? state.items.concat(mapped) : mapped;
      const seen = new Set();
      state.items = combined.filter((item) => {
        if (!item.id || seen.has(item.id)) return false;
        seen.add(item.id);
        return true;
      });
      const addedCount = state.items.length - previousCount;
      state.pageCount = append ? state.pageCount + 1 : 1;
      state.cursor = data.cursor || data.next_cursor || "";
      state.hasMore = !!data.has_more && !!state.cursor;
      if (append && addedCount === 0) state.hasMore = false;

      if (view === "rank") {
        if (Array.isArray(data.contents)) {
          state.rankContents = data.contents.filter((content) => content && content.id && content.name !== "演员");
        }
        if (data.content) state.rankContent = data.content;
        if (data.board) state.rankBoard = data.board;
        state.activeViewKey = viewCacheKey(view);
      } else if (view === "new") {
        if (Array.isArray(data.dates)) state.newDates = data.dates.map((value) => String(value).replace(/-/g, ""));
        if (data.date) state.newDate = String(data.date).replace(/-/g, "");
        state.activeViewKey = viewCacheKey(view);
      }

      renderFilterTags();
      renderCards(append ? state.items.slice(previousCount) : state.items, append);
      if (!append && view === "discover") renderHero(state.items);
      renderPagination();
      setStatus(state.items.length
        ? ("已载入 " + state.items.length + " 部" + (state.pageCount > 1 ? " · 第 " + state.pageCount + " 页" : ""))
        : "暂无内容");
      saveActiveView();
      loadSucceeded = true;
    } catch (error) {
      if (loadID !== state.loadSequence || activeKey !== state.activeViewKey) return;
      setStatus(error.message || "加载失败", true);
      if (!append) $("#series-grid").innerHTML = '<div class="empty-state">片库暂时没有响应。<br>请检查 N1 网络后点击刷新。</div>';
    } finally {
      if (loadID === state.loadSequence) {
        setBusy(false);
        renderPagination();
        // Match 开心果的预加载手感: load two pages before waiting for scroll,
        // then continue when the viewer comes within roughly one screen of the end.
        const scheduledView = state.view;
        const scheduledKey = state.activeViewKey;
        if (loadSucceeded && state.hasMore && (state.pageCount < 2 || isNearPageEnd())) {
          window.setTimeout(() => {
            if (state.view === scheduledView && state.activeViewKey === scheduledKey &&
                !state.busy && state.hasMore && (state.pageCount < 2 || isNearPageEnd())) {
              loadView(scheduledView, true);
            }
          }, 120);
        }
      }
    }
  }

  function isNearPageEnd() {
    const remaining = document.documentElement.scrollHeight - (window.scrollY + window.innerHeight);
    return remaining <= Math.max(750, window.innerHeight * 0.85);
  }

  function maybeAutoLoadMore() {
    if (!state.hasMore || state.busy || !["discover", "search", "rank", "new"].includes(state.view)) return;
    if (isNearPageEnd()) loadView(state.view, true);
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
      const title = series.title || "贝小果短剧";
      const cover = safeImage(series.cover_url || series.coverUrl);
      const fav = state.favorites.some((entry) => entry.id === id);
      state.playback = {seriesId:id,seriesTitle:title,episodes:episodes,index:-1,quality:"best",speed:1,requestId:0};
      body.innerHTML =
        '<div class="detail-hero">' +
        (cover ? '<img src="' + escapeHTML(cover) + '" alt="">' : '<div class="poster-placeholder detail-placeholder">贝</div>') +
        '<div class="detail-copy"><div class="detail-meta">' + escapeHTML(series.right_text || series.subtitles || "贝小果片库") + '</div>' +
        '<h2>' + escapeHTML(title) + '</h2>' +
        '<div class="detail-meta">' + escapeHTML(episodes.length + " 集") + '</div>' +
        '<p>' + escapeHTML(series.description || "暂无简介") + '</p>' +
        '<button id="favorite-toggle" class="load-more" type="button">' + (fav ? "♥ 已收藏" : "♡ 收藏这部剧") + '</button></div></div>' +
        '<section id="playback-panel" class="playback-panel" hidden>' +
        '<div class="player-toolbar"><span id="player-caption">正在准备播放…</span>' +
        '<label class="quality-control">画质 <select id="player-quality" aria-label="播放画质"><option value="best">自动</option></select></label>' +
        '<label class="quality-control">倍速 <select id="player-speed" aria-label="播放速度">' +
        '<option value="0.5">0.5×</option><option value="0.75">0.75×</option><option value="1" selected>1×</option>' +
        '<option value="1.25">1.25×</option><option value="1.5">1.5×</option><option value="2">2×</option>' +
        '<option value="2.5">2.5×</option><option value="3">3×</option></select></label>' +
        '<button id="player-cast" class="player-action" type="button" title="投屏到 hongguoTV">投屏</button>' +
        '<button id="player-fullscreen" class="player-action" type="button">全屏</button>' +
        '<button id="player-close" class="player-close" type="button">收起</button></div>' +
        '<div id="player-stage" class="player-stage"><video id="web-player" controls playsinline webkit-playsinline disableRemotePlayback x-webkit-airplay="deny" preload="metadata"></video>' +
        '<div class="swipe-hint">上滑下一集 · 下滑上一集</div></div>' +
        '<div class="player-nav"><button id="player-prev" type="button">上一集</button><button id="player-next" type="button">下一集</button></div>' +
        '<div id="playback-status" class="playback-status" role="status">选择集数开始播放</div></section>' +
        '<h3 class="episode-heading">选集</h3><div class="episode-grid">' +
        episodes.map((ep) => '<button class="episode-chip" type="button" data-episode-id="' + escapeHTML(ep.id) + '" data-episode-number="' + escapeHTML(ep.number || "") + '"' + (ep.playable === false ? ' disabled title="当前分集不可播放"' : '') + '>' + escapeHTML("第 " + (ep.number || "") + " 集") + '</button>').join("") +
        '</div><div class="player-note">支持 0.5–3 倍速、上下滑切集和自动连播。视频仍由 N1 按需读取与解密；投屏通过局域网交给 hongguoTV。</div>';

      $("#favorite-toggle").addEventListener("click", () => toggleFavorite(normalize(series)));
      body.querySelector(".episode-grid").addEventListener("click", (event) => {
        const button = event.target.closest(".episode-chip");
        if (!button || button.disabled) return;
        const index = episodes.findIndex((ep) => String(ep.id) === button.dataset.episodeId);
        if (index >= 0) playEpisodeAt(index);
      });
      $("#player-close").addEventListener("click", () => {
        stopPlayback();
        $("#playback-panel").hidden = true;
      });
      $("#player-prev").addEventListener("click", () => changeEpisode(-1));
      $("#player-next").addEventListener("click", () => changeEpisode(1));
      $("#player-cast").addEventListener("click", openCastPicker);
      $("#player-speed").addEventListener("change", (event) => {
        state.playback.speed = Number(event.target.value) || 1;
        $("#web-player").playbackRate = state.playback.speed;
      });
      $("#player-quality").addEventListener("change", () => {
        const video = $("#web-player");
        const position = Number.isFinite(video.currentTime) ? video.currentTime : 0;
        startPlayback(state.playback.index, $("#player-quality").value, position);
      });
      const video = $("#web-player");
      video.addEventListener("ended", () => {
        if (state.playback.index >= 0) {
          const next = findPlayableEpisode(state.playback.index, 1);
          if (next >= 0) {
            showToast("本集播放完毕，自动播放下一集");
            playEpisodeAt(next);
          } else {
            $("#playback-status").textContent = "本剧已播放完毕";
          }
        }
      });
      let touchStart = null;
      $("#player-stage").addEventListener("touchstart", (event) => {
        if (event.touches.length !== 1) return;
        touchStart = {x:event.touches[0].clientX,y:event.touches[0].clientY};
      }, {passive:true});
      $("#player-stage").addEventListener("touchend", (event) => {
        if (!touchStart || !event.changedTouches.length) return;
        const dx = event.changedTouches[0].clientX - touchStart.x;
        const dy = event.changedTouches[0].clientY - touchStart.y;
        touchStart = null;
        if (Math.abs(dy) < 55 || Math.abs(dy) < Math.abs(dx) * 1.25) return;
        changeEpisode(dy < 0 ? 1 : -1);
      }, {passive:true});
      $("#player-fullscreen").addEventListener("click", async () => {
        const panel = $("#playback-panel");
        const button = $("#player-fullscreen");
        if (panel.classList.contains("manual-fullscreen")) {
          panel.classList.remove("manual-fullscreen");
          button.textContent = "全屏";
          return;
        }
        try {
          if (panel.requestFullscreen) {
            await panel.requestFullscreen();
          } else {
            panel.classList.add("manual-fullscreen");
            button.textContent = "退出全屏";
          }
        } catch (_) {
          panel.classList.add("manual-fullscreen");
          button.textContent = "退出全屏";
        }
      });
      document.addEventListener("fullscreenchange", () => {
        const button = $("#player-fullscreen");
        if (button && !$("#playback-panel")?.classList.contains("manual-fullscreen")) {
          button.textContent = document.fullscreenElement ? "退出全屏" : "全屏";
        }
      });
    } catch (error) {
      body.innerHTML = '<div class="empty-state">' + escapeHTML(error.message || "详情载入失败") + '</div>';
    }
  }

  function findPlayableEpisode(from, direction) {
    const episodes = state.playback.episodes || [];
    for (let index = from + direction; index >= 0 && index < episodes.length; index += direction) {
      if (episodes[index] && episodes[index].playable !== false && episodes[index].id) return index;
    }
    return -1;
  }

  function changeEpisode(direction) {
    const index = findPlayableEpisode(state.playback.index, direction);
    if (index < 0) {
      showToast(direction > 0 ? "已经是最后一集" : "已经是第一集");
      return;
    }
    playEpisodeAt(index);
  }

  function playEpisodeAt(index) {
    const episode = state.playback.episodes[index];
    if (!episode || episode.playable === false) return;
    state.playback.index = index;
    startPlayback(index, state.playback.quality, 0);
  }

  async function startPlayback(index, quality, resumeAt) {
    const playback = state.playback;
    const episode = playback.episodes[index];
    if (!episode || !episode.id) return;
    playback.index = index;
    const requestID = ++playback.requestId;
    const episodeNumber = episode.number || index + 1;
    const panel = $("#playback-panel");
    const video = $("#web-player");
    const status = $("#playback-status");
    const caption = $("#player-caption");
    const qualitySelect = $("#player-quality");
    const speedSelect = $("#player-speed");
    panel.hidden = false;
    caption.textContent = playback.seriesTitle + " · 第 " + episodeNumber + " 集";
    status.textContent = "正在解析播放地址…";
    video.pause();
    video.removeAttribute("src");
    video.load();

    try {
      const chosen = quality || playback.quality || "best";
      const info = await request("/api/v1/play/" + encodeURIComponent(episode.id) + "?quality=" + encodeURIComponent(chosen));
      if (requestID !== playback.requestId) return;
      playback.quality = info.quality || chosen;
      qualitySelect.innerHTML = (info.available_qualities || []).map((item) =>
        '<option value="' + escapeHTML(item.label) + '">' + escapeHTML(item.label) + (item.codec ? " · " + escapeHTML(item.codec) : "") + '</option>'
      ).join("");
      if (!qualitySelect.options.length) qualitySelect.innerHTML = '<option value="best">自动</option>';
      qualitySelect.value = playback.quality;
      speedSelect.value = String(playback.speed || 1);

      status.textContent = "已取得 " + (info.quality || "自动") + " 播放流" +
        (info.protected ? " · N1 正在按需解密" : " · 明文 MP4") + "。正在连接视频…";
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
      video.playbackRate = playback.speed || 1;
      try {
        await video.play();
        if (requestID === playback.requestId) {
          status.textContent = "正在播放 · " + (info.quality || "自动") + " · " + video.playbackRate + "×";
        }
      } catch (_) {
        if (requestID === playback.requestId) status.textContent = "播放地址已就绪，点击视频播放键开始。";
      }
      updateEpisodeButtons();
    } catch (error) {
      if (requestID !== playback.requestId) return;
      status.textContent = error.message || "播放初始化失败";
      showToast(error.message || "播放初始化失败");
    }
  }

  function updateEpisodeButtons() {
    const playback = state.playback;
    const previous = $("#player-prev");
    const next = $("#player-next");
    if (previous) previous.disabled = findPlayableEpisode(playback.index, -1) < 0;
    if (next) next.disabled = findPlayableEpisode(playback.index, 1) < 0;
    document.querySelectorAll(".episode-chip").forEach((button) => {
      button.classList.toggle("playing", button.dataset.episodeId === String(playback.episodes[playback.index]?.id || ""));
    });
  }

  async function openCastPicker() {
    const playback = state.playback;
    const episode = playback.episodes[playback.index];
    if (!episode || !playback.seriesId) {
      showToast("请先播放一集，再投屏到电视");
      return;
    }
    const dialog = $("#cast-dialog");
    const content = $("#cast-dialog-content");
    content.innerHTML = '<div class="empty-state">正在搜索局域网内的 hongguoTV…</div>';
    if (!dialog.open) dialog.showModal();
    try {
      const result = await request("/api/v1/tv/devices");
      const devices = result.devices || [];
      if (!devices.length) {
        content.innerHTML = '<div class="cast-empty">没有发现电视。请确认电视已打开 hongguoTV，并且 N1、手机和电视在同一局域网，路由器允许 mDNS 组播。</div>';
        return;
      }
      content.innerHTML = devices.map((device) =>
        '<button type="button" class="cast-device" data-device-id="' + escapeHTML(device.device_id) + '">' +
        '<span class="cast-tv-icon">▣</span><span><strong>' + escapeHTML(device.name || "hongguoTV") + '</strong>' +
        '<small>' + escapeHTML([device.manufacturer,device.model,device.code ? "编号 " + device.code : ""].filter(Boolean).join(" · ")) + '</small></span><span class="cast-chevron">›</span></button>'
      ).join("");
      content.querySelectorAll(".cast-device").forEach((button) => {
        button.addEventListener("click", async () => {
          content.querySelectorAll("button").forEach((item) => { item.disabled = true; });
          button.classList.add("sending");
          const position = Math.max(0, Number($("#web-player").currentTime) || 0);
          try {
            const reply = await request("/api/v1/tv/handoff", {
              method:"POST",
              headers:{"Content-Type":"application/json"},
              body:JSON.stringify({
                device_id:button.dataset.deviceId,
                series_id:playback.seriesId,
                episode:Number(episode.number) || playback.index + 1,
                position_ms:Math.round(position * 1000)
              })
            });
            if (reply.accepted) {
              $("#web-player").pause();
              showToast("已交给 " + (reply.device_name || "电视") + " 播放");
              dialog.close();
            } else {
              button.disabled = false;
              showToast(reply.message || "电视没有接受播放任务");
            }
          } catch (error) {
            button.disabled = false;
            showToast(error.message || "投屏失败");
          }
        });
      });
    } catch (error) {
      content.innerHTML = '<div class="cast-empty">' + escapeHTML(error.message || "电视搜索失败") + '</div>';
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
    saveActiveView();
    state.searchType = "all";
    loadView("search", false, true);
  });
  $("#refresh-button").addEventListener("click", () => loadView(state.view, false, true));
  $("#load-more").addEventListener("click", () => loadView(state.view, true));
  $("#filter-tags").addEventListener("click", (event) => {
    const button = event.target.closest(".filter-tag");
    if (!button) return;
    const kind = button.dataset.filterKind;
    const value = button.dataset.filterValue;
    if (!kind || value == null) return;
    const changing =
      (kind === "channel" && value !== state.discoverChannel) ||
      (kind === "search-type" && value !== state.searchType) ||
      (kind === "rank-content" && value !== state.rankContent) ||
      (kind === "rank-board" && value !== state.rankBoard) ||
      (kind === "new-date" && value !== state.newDate);
    if (changing) saveActiveView();
    if (kind === "channel" && value !== state.discoverChannel) {
      state.discoverChannel = value;
      loadView("discover", false, true);
    } else if (kind === "search-type" && value !== state.searchType) {
      state.searchType = value;
      loadView("search", false, true);
    } else if (kind === "rank-content" && value !== state.rankContent) {
      state.rankContent = value;
      state.rankBoard = "";
      loadView("rank", false, true);
    } else if (kind === "rank-board" && value !== state.rankBoard) {
      state.rankBoard = value;
      loadView("rank", false, true);
    } else if (kind === "new-date" && value !== state.newDate) {
      state.newDate = value;
      loadView("new", false, true);
    }
  });
  window.addEventListener("scroll", () => {
    if (state.autoLoadScheduled) return;
    state.autoLoadScheduled = true;
    window.requestAnimationFrame(() => {
      state.autoLoadScheduled = false;
      maybeAutoLoadMore();
    });
  }, {passive:true});
  window.addEventListener("resize", maybeAutoLoadMore, {passive:true});
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
  $("#cast-dialog-close").addEventListener("click", () => $("#cast-dialog").close());

  loadView("discover", false);
})();
