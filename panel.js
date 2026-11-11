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
    const next = page.querySelector('.device-board');
    if (!next) throw new Error('一覧を更新できませんでした。');
    const position = { x: scrollX, y: scrollY };
    document.querySelector('.device-board').replaceWith(next);
    arrangeDevices();
    document.querySelector('.device-count').textContent = page.querySelector('.device-count').textContent;
    submitButton(form).title = submitButton(page.querySelector('.refresh')).title;
    restore();
    scrollTo(position.x, position.y);
  }

  document.addEventListener('submit', async event => {
    const form = event.target;
    if (!form.matches('.commands form, .refresh')) return;
    event.preventDefault();
    if (pending.has(form) || editing) return;
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

  const layoutKey = 'switchbot-panel-layout-v1';
  const layoutTools = document.getElementById('layout-tools');
  const layoutError = document.getElementById('layout-error');
  const ungrouped = '未分類';
  const emptyLayout = () => [{ name: ungrouped, ids: [] }];
  let layout = readLayout();
  let editing;
  let drag;

  function readLayout() {
    try {
      const groups = JSON.parse(localStorage.getItem(layoutKey));
      if (Array.isArray(groups) && groups.length && groups.every(group =>
        group && typeof group.name === 'string' && Array.isArray(group.ids) &&
        group.ids.every(id => typeof id === 'string'))) return groups;
    } catch (_) { /* Start with the default layout if storage is unavailable. */ }
    return emptyLayout();
  }

  function withNewDevices(groups, ids) {
    const assigned = new Set(groups.flatMap(group => group.ids));
    groups[0].ids.push(...ids.filter(id => !assigned.has(id)));
    return groups;
  }

  function arrangeDevices(focusId) {
    const board = document.querySelector('.device-board');
    const metadata = board.querySelector('.layout-devices');
    const cards = new Map([...board.querySelectorAll('.device')].map(card => [card.dataset.deviceId, card]));
    const ids = [...metadata.content.querySelectorAll('[data-id]')].map(device => device.dataset.id);
    const customLayout = layout.length > 1 || layout[0].ids.length > 0;
    const groups = editing || withNewDevices(structuredClone(layout), ids);
    const sections = [];

    board.classList.toggle('editing-layout', Boolean(editing));
    for (const index of [...groups.keys()].slice(1).concat(0)) {
      const group = groups[index];
      const list = document.createElement('div');
      list.className = editing || customLayout ? 'device-list arranged' : 'device-list';
      for (const id of group.ids) {
        const card = cards.get(id);
        if (!card) continue;
        card.querySelector('.layout-handle')?.remove();
        for (const child of card.children) child.inert = Boolean(editing);
        if (editing) card.prepend(dragHandle(card, index));
        list.append(card);
        cards.delete(id);
      }
      if (!editing && !list.childElementCount) continue;
      if (!editing && groups.length === 1) {
        sections.push(list);
        continue;
      }
      const section = document.createElement('section');
      section.className = 'layout-group';
      section.dataset.layoutGroup = index;
      if (editing && index !== 0) {
        const controls = document.createElement('div');
        controls.className = 'layout-group-controls';
        const name = document.createElement('input');
        name.value = group.name;
        name.placeholder = 'グループ名';
        name.maxLength = 60;
        name.setAttribute('aria-label', 'グループ名');
        name.addEventListener('input', () => { group.name = name.value; });
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.textContent = 'グループを削除';
        remove.addEventListener('click', () => {
          editing[0].ids.push(...group.ids);
          editing.splice(index, 1);
          arrangeDevices();
        });
        controls.append(name, remove);
        section.append(controls);
      } else {
        const heading = document.createElement('h2');
        heading.textContent = group.name;
        section.append(heading);
      }
      section.append(list);
      sections.push(section);
    }
    const empty = board.querySelector('.empty') || document.createElement('div');
    empty.className = 'empty';
    empty.textContent = '該当する機器がありません。';
    board.replaceChildren(metadata, ...(sections.length ? sections : [empty]));
    if (focusId) document.getElementById('device-' + focusId)?.querySelector('.layout-handle')?.focus({ preventScroll: true });
  }

  function moveCard(id, from, to, relativeTo, after) {
    const source = editing[from].ids;
    source.splice(source.indexOf(id), 1);
    const destination = editing[to].ids;
    const position = relativeTo ? destination.indexOf(relativeTo) + Number(after) : destination.length;
    destination.splice(position, 0, id);
    arrangeDevices(id);
  }

  function dragHandle(card, index) {
    const handle = document.createElement('button');
    const id = card.dataset.deviceId;
    handle.type = 'button';
    handle.className = 'layout-handle';
    handle.textContent = '⠿ 移動';
    handle.setAttribute('aria-label', card.querySelector('.name').textContent + 'を移動');
    handle.setAttribute('aria-description', '上下キーで順番を変更、左右キーでグループを移動');
    handle.addEventListener('pointerdown', event => {
      if (!event.isPrimary || event.button !== 0 || drag) return;
      handle.setPointerCapture(event.pointerId);
      drag = { id, index, card, pointer: event.pointerId, x: event.clientX, y: event.clientY };
    });
    handle.addEventListener('pointermove', event => {
      if (drag?.pointer !== event.pointerId) return;
      if (!drag.active && Math.hypot(event.clientX - drag.x, event.clientY - drag.y) < 5) return;
      if (!drag.active) {
        drag.active = true;
        card.classList.add('layout-dragging');
        drag.frame = requestAnimationFrame(scrollDrag);
      }
      drag.x = event.clientX;
      drag.y = event.clientY;
      updateDropTarget();
    });
    handle.addEventListener('pointerup', event => {
      if (drag?.pointer !== event.pointerId) return;
      if (drag.active) {
        drag.x = event.clientX;
        drag.y = event.clientY;
        updateDropTarget();
      }
      const target = drag.target;
      clearDrag();
      if (target) moveCard(id, index, target.index, target.id, target.after);
    });
    handle.addEventListener('lostpointercapture', () => {
      if (drag?.card === card) clearDrag();
    });
    handle.addEventListener('keydown', event => {
      if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
        event.preventDefault();
        const order = [...editing.keys()].slice(1).concat(0);
        const to = order[order.indexOf(index) + (event.key === 'ArrowLeft' ? -1 : 1)];
        if (to !== undefined) moveCard(id, index, to);
        return;
      }
      const cards = [...card.parentElement.querySelectorAll('.device')];
      const position = cards.indexOf(card);
      const offset = event.key === 'ArrowUp' ? -1 : event.key === 'ArrowDown' ? 1 : 0;
      if (offset) {
        event.preventDefault();
        const neighbor = cards[position + offset];
        if (neighbor) moveCard(id, index, index, neighbor.dataset.deviceId, offset > 0);
      }
    });
    return handle;
  }

  function updateDropTarget() {
    drag.marker?.classList.remove(drag.markerClass);
    drag.target = null;
    const hit = document.elementFromPoint(drag.x, drag.y);
    const section = hit?.closest('[data-layout-group]');
    const card = hit?.closest('.device');
    if (!section || card === drag.card) return;
    const rect = card?.getBoundingClientRect();
    const multiColumn = card && card.parentElement.clientWidth > rect.width + 20;
    const after = card && (multiColumn ? drag.x > rect.left + rect.width / 2 : drag.y > rect.top + rect.height / 2);
    drag.target = { index: Number(section.dataset.layoutGroup), id: card?.dataset.deviceId, after };
    drag.marker = card || section;
    if (multiColumn) drag.markerClass = after ? 'layout-drop-right' : 'layout-drop-left';
    else drag.markerClass = card ? (after ? 'layout-drop-after' : 'layout-drop-before') : 'layout-drop-group';
    drag.marker.classList.add(drag.markerClass);
  }

  function scrollDrag() {
    if (!drag?.active) return;
    const step = drag.y < 70 ? -12 : drag.y > innerHeight - 70 ? 12 : 0;
    if (step) {
      scrollBy(0, step);
      updateDropTarget();
    }
    drag.frame = requestAnimationFrame(scrollDrag);
  }

  function clearDrag() {
    cancelAnimationFrame(drag.frame);
    drag.marker?.classList.remove(drag.markerClass);
    drag.card.classList.remove('layout-dragging');
    drag = null;
  }

  const openLayout = document.getElementById('layout-open');
  const refreshButton = submitButton(document.querySelector('.refresh'));
  openLayout.hidden = false;
  openLayout.addEventListener('click', () => {
    if (refreshing || commandCount || editing) return;
    const devices = document.querySelector('.layout-devices').content.querySelectorAll('[data-id]');
    editing = withNewDevices(structuredClone(layout), [...devices].map(device => device.dataset.id));
    openLayout.hidden = true;
    refreshButton.disabled = true;
    layoutTools.hidden = false;
    layoutError.hidden = true;
    arrangeDevices();
    document.getElementById('layout-add').focus();
  });

  function closeLayout() {
    if (drag) clearDrag();
    editing = null;
    layoutTools.hidden = true;
    openLayout.hidden = false;
    refreshButton.disabled = false;
    arrangeDevices();
    openLayout.focus();
  }

  document.getElementById('layout-add').addEventListener('click', () => {
    editing.push({ name: '', ids: [] });
    arrangeDevices();
    document.querySelector('[data-layout-group="' + (editing.length - 1) + '"] input').focus();
  });
  document.getElementById('layout-cancel').addEventListener('click', closeLayout);
  document.getElementById('layout-save').addEventListener('click', () => {
    const names = editing.slice(1).map(group => group.name.trim());
    if (names.some(name => !name || name === ungrouped) || new Set(names).size !== names.length) {
      layoutError.textContent = 'グループには重複しない名前を入力してください。';
      layoutError.hidden = false;
      return;
    }
    for (const group of editing) group.name = group.name.trim();
    try {
      localStorage.setItem(layoutKey, JSON.stringify(editing));
    } catch (_) {
      layoutError.textContent = 'このブラウザに配置を保存できませんでした。';
      layoutError.hidden = false;
      return;
    }
    layout = editing;
    closeLayout();
  });

  arrangeDevices();
  restore();
  // History navigation can restore a document without re-running its script.
  addEventListener('pageshow', restore);
})();
