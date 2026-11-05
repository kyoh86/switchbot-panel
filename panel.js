(() => {
  const storageKey = 'switchbot-panel-drafts-v1';
  let drafts = {};
  try {
    const saved = JSON.parse(sessionStorage.getItem(storageKey) || '{}');
    if (saved && typeof saved === 'object' && !Array.isArray(saved)) drafts = saved;
  } catch (_) { /* Drafts still work in memory when storage is unavailable. */ }

  const pending = new WeakSet();
  let commandCount = 0;
  let refreshing = false;
  const key = form => form.getAttribute('action') + '|' + form.elements.action.value;
  const controls = form => [...form.elements].filter(element =>
    element.name && element.type !== 'hidden' && element.type !== 'submit');
  const submitButton = form => form.querySelector('button[type="submit"]');

  function persist() {
    try { sessionStorage.setItem(storageKey, JSON.stringify(drafts)); } catch (_) {}
  }

  function feedback(form, message, error = false) {
    let box = form.matches('.refresh') ? document.getElementById('page-feedback') : form.querySelector('.feedback');
    if (!box) {
      box = document.createElement('p');
      box.className = 'feedback';
      box.setAttribute('role', 'status');
      box.setAttribute('aria-live', 'polite');
      form.append(box);
    }
    box.hidden = false;
    box.dataset.kind = error ? 'error' : 'success';
    box.textContent = message;
  }

  function draftReadout(form) {
    const output = form.querySelector('output');
    const value = form.querySelector('input[type="range"], input[type="color"]');
    if (!value) return;
    if (value.type === 'color') {
      form.querySelector('.color-picker').style.setProperty('--picked-color', value.value);
      if (output) output.textContent = '';
    } else if (output) {
      const rounded = Math.round(Number(value.value) / 100) * 100;
      const display = value.dataset.unit === 'K' && rounded !== Number(value.value) ? '約' + rounded : value.value;
      output.textContent = display + (value.dataset.unit ? ' ' + value.dataset.unit : '');
    }
  }

  function save(form) {
    const values = {};
    for (const element of controls(form)) {
      if (element.type === 'radio') {
        if (!(element.name in values)) values[element.name] = '';
        if (element.checked) values[element.name] = element.value;
      } else {
        values[element.name] = element.value;
      }
    }
    drafts[key(form)] = values;
    form.dataset.dirty = 'true';
    form.dataset.revision = String(Number(form.dataset.revision || 0) + 1);
    persist();
  }

  function restore() {
    for (const form of document.querySelectorAll('.commands form')) {
      const values = drafts[key(form)];
      if (!values || typeof values !== 'object') continue;
      for (const element of controls(form)) {
        if (!Object.hasOwn(values, element.name)) continue;
        if (element.type === 'radio') element.checked = element.value === values[element.name];
        else element.value = values[element.name];
      }
      draftReadout(form);
      form.dataset.dirty = 'true';
      submitButton(form).disabled = !form.checkValidity();
    }
  }

  function edit(event) {
    const form = event.target.form;
    if (!form || !form.matches('.setting-control')) return;
    save(form);
    draftReadout(form);
    submitButton(form).disabled = pending.has(form) || !form.checkValidity();
  }
  document.addEventListener('input', edit);
  document.addEventListener('change', edit);

  async function post(form) {
    const response = await fetch(form.getAttribute('action'), {
      method: 'POST',
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      body: new URLSearchParams(new FormData(form)),
    });
    if (!response.ok) {
      throw new Error((await response.text()).trim() || '操作に失敗しました。');
    }
    await response.json();
  }

  async function refreshList(form) {
    await post(form);
    const response = await fetch(location.pathname + location.search, { credentials: 'same-origin' });
    if (!response.ok) throw new Error('一覧を取得できませんでした。現在の表示と編集中の設定は保持しています。');
    const page = new DOMParser().parseFromString(await response.text(), 'text/html');
    const next = page.querySelector('.device-list');
    if (!next) throw new Error('一覧を更新できませんでした。');
    const position = { x: scrollX, y: scrollY };
    document.querySelector('.device-list').replaceWith(next);
    document.querySelector('.device-count').textContent = page.querySelector('.device-count').textContent;
    submitButton(form).title = submitButton(page.querySelector('.refresh')).title;
    restore();
    scrollTo(position.x, position.y);
  }

  document.addEventListener('submit', async event => {
    const form = event.target;
    if (!form.matches('.commands form, .refresh')) return;
    event.preventDefault();
    if (pending.has(form)) return;
    const isRefresh = form.matches('.refresh');
    if (refreshing || (isRefresh && commandCount > 0)) {
      feedback(form, '処理中です。完了してから操作してください。');
      return;
    }
    if (!form.checkValidity()) return;
    const revision = form.dataset.revision || '0';
    const isSetting = form.matches('.setting-control');
    const buttonLabel = submitButton(form).textContent;
    pending.add(form);
    if (isRefresh) refreshing = true;
    else commandCount++;
    submitButton(form).disabled = true;
    if (isSetting) submitButton(form).textContent = '適用中…';
    if (isRefresh) submitButton(form).textContent = '更新中…';
    form.setAttribute('aria-busy', 'true');
    submitButton(document.querySelector('.refresh')).disabled = true;
    const previousFeedback = isRefresh ? document.getElementById('page-feedback') : form.querySelector('.feedback');
    if (previousFeedback) previousFeedback.hidden = true;
    try {
      if (isRefresh) {
        await refreshList(form);
      } else {
        await post(form);
        // A newer edit made while the request was in flight remains a draft.
        if ((form.dataset.revision || '0') === revision) {
          delete drafts[key(form)];
          delete form.dataset.dirty;
          persist();
        }
        const box = form.querySelector('.feedback');
        if (box) {
          box.textContent = '';
          box.hidden = true;
        }
      }
    } catch (error) {
      feedback(form, error instanceof TypeError ? '通信できませんでした。設定は保持しています。' : error.message, true);
    } finally {
      pending.delete(form);
      if (isRefresh) refreshing = false;
      else commandCount--;
      form.removeAttribute('aria-busy');
      if (isSetting || isRefresh) submitButton(form).textContent = buttonLabel;
      submitButton(form).disabled = (isSetting && !form.dataset.dirty) || !form.checkValidity();
      submitButton(document.querySelector('.refresh')).disabled = refreshing || commandCount > 0;
    }
  });

  restore();
  // History navigation can restore a document without re-running its script.
  addEventListener('pageshow', restore);
})();
