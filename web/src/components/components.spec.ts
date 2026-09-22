/**
 * Tests for the hand-built controls.
 *
 * The dropdown is the one that has to be right: a native `<select>` gets
 * keyboard navigation, dismissal and screen reader support from the browser,
 * and every one of those is something a replacement has to reimplement. These
 * check the behaviours a user would notice missing — not the styling, which is
 * visible on screen and does not need asserting.
 *
 * The wrapped controls are tested for the opposite reason: the point of
 * wrapping a checkbox is that the native element is still there doing the
 * work, so what is asserted is that it still is.
 */

import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { beforeAll, describe, expect, it } from 'vitest'

import AppCheckbox from './AppCheckbox.vue'
import AppSelect from './AppSelect.vue'

// jsdom has neither of these, and the dropdown scrolls its highlight into
// view on every move.
beforeAll(() => {
  Element.prototype.scrollIntoView = () => {}
  Element.prototype.getBoundingClientRect = () =>
    ({ top: 0, bottom: 20, left: 0, right: 200, width: 200, height: 20, x: 0, y: 0 }) as DOMRect
})

const OPTIONS = [
  { value: 'tiny', label: 'tiny — 最快' },
  { value: 'medium', label: 'medium — 默认' },
  { value: 'large-v3', label: 'large-v3 — 最准' },
]

function mountSelect(modelValue = 'medium') {
  return mount(AppSelect, {
    props: { options: OPTIONS, modelValue, id: 'model' },
    attachTo: document.body,
  })
}

/** The listbox, which is teleported out of the component's own subtree. */
function list(): HTMLElement | null {
  return document.body.querySelector('[role="listbox"]')
}

function options(): HTMLElement[] {
  return Array.from(document.body.querySelectorAll('[role="option"]'))
}

describe('AppSelect', () => {
  it('shows the label of the selected option, not its value', () => {
    const wrapper = mountSelect()

    expect(wrapper.get('[role="combobox"]').text()).toContain('medium — 默认')
    expect(wrapper.get('[role="combobox"]').text()).not.toContain('medium,')
  })

  it('is not a native select', () => {
    const wrapper = mount(AppSelect, { props: { options: OPTIONS, modelValue: 'tiny' } })

    // The whole reason this component exists: a native popup is drawn by the
    // operating system and no stylesheet reaches it.
    expect(wrapper.find('select').exists()).toBe(false)
    expect(wrapper.find('option').exists()).toBe(false)
  })

  it('is closed until it is asked for', () => {
    const wrapper = mountSelect()

    expect(list()).toBeNull()
    expect(wrapper.get('[role="combobox"]').attributes('aria-expanded')).toBe('false')
  })

  it('opens on a click and on the keyboard', async () => {
    const clicked = mountSelect()
    await clicked.get('[role="combobox"]').trigger('click')
    expect(list()).not.toBeNull()
    clicked.unmount()

    const pressed = mountSelect()
    await pressed.get('[role="combobox"]').trigger('keydown', { key: 'ArrowDown' })
    expect(list()).not.toBeNull()
    pressed.unmount()
  })

  it('moves the highlight with the arrow keys and wraps', async () => {
    const wrapper = mountSelect('medium')
    await wrapper.get('[role="combobox"]').trigger('click')

    // Opens on the current selection rather than on the first option, which is
    // what a native select does.
    const highlighted = () => options().findIndex((o) => o.className.includes('bg-surface'))

    expect(highlighted()).toBe(1)

    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'ArrowDown' })
    expect(highlighted()).toBe(2)

    // Past the end wraps to the start.
    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'ArrowDown' })
    expect(highlighted()).toBe(0)

    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'ArrowUp' })
    expect(highlighted()).toBe(2)

    wrapper.unmount()
  })

  it('selects the highlighted option on Enter and closes', async () => {
    const wrapper = mountSelect('tiny')
    await wrapper.get('[role="combobox"]').trigger('click')
    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'ArrowDown' })
    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'Enter' })

    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['medium'])
    expect(list()).toBeNull()

    wrapper.unmount()
  })

  it('closes on Escape without changing anything', async () => {
    const wrapper = mountSelect('tiny')
    await wrapper.get('[role="combobox"]').trigger('click')
    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'ArrowDown' })
    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'Escape' })

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(list()).toBeNull()

    wrapper.unmount()
  })

  it('jumps to an option by typing', async () => {
    const wrapper = mountSelect('tiny')
    await wrapper.get('[role="combobox"]').trigger('click')

    // "l" reaches large-v3, which is the only option starting with it.
    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'l' })
    await wrapper.get('[role="combobox"]').trigger('keydown', { key: 'Enter' })

    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['large-v3'])

    wrapper.unmount()
  })

  it('closes when the pointer goes down outside it, and stays open inside', async () => {
    const wrapper = mountSelect()
    await wrapper.get('[role="combobox"]').trigger('click')
    expect(list()).not.toBeNull()

    // A plain Event rather than a PointerEvent: jsdom constructs the latter
    // but does not dispatch it through the capture listener, and the handler
    // reads nothing from the event but its target.
    //
    // The await is for Vue rather than for the event: dismissing sets a ref,
    // and the DOM is patched on the next tick — so checking immediately reads
    // the old tree and reports a bug that is not there.
    const press = async (target: Element) => {
      target.dispatchEvent(new Event('pointerdown', { bubbles: true }))
      await nextTick()
    }

    // A press on the list itself must not dismiss it, or selecting with the
    // mouse would close the menu before the click that chooses lands.
    await press(options()[2]!)
    expect(list()).not.toBeNull()

    await press(document.body)
    expect(list()).toBeNull()

    wrapper.unmount()
  })

  it('does not open while disabled', async () => {
    const wrapper = mount(AppSelect, {
      props: { options: OPTIONS, modelValue: 'tiny', disabled: true },
      attachTo: document.body,
    })

    await wrapper.get('[role="combobox"]').trigger('click')
    expect(list()).toBeNull()

    wrapper.unmount()
  })

  it('carries the roles a screen reader needs', async () => {
    const wrapper = mountSelect()
    await wrapper.get('[role="combobox"]').trigger('click')

    expect(wrapper.get('[role="combobox"]').attributes('aria-haspopup')).toBe('listbox')
    // aria-expanded is what announces "collapsed" or "expanded"; without it the
    // trigger reads as a button that does nothing.
    expect(wrapper.get('[role="combobox"]').attributes('aria-expanded')).toBe('true')

    const selected = options().filter((o) => o.getAttribute('aria-selected') === 'true')
    expect(selected).toHaveLength(1)
    expect(selected[0]?.textContent).toContain('medium')

    wrapper.unmount()
  })
})

describe('AppCheckbox', () => {
  it('still uses a native input, which is what carries the keyboard and the label', () => {
    const wrapper = mount(AppCheckbox, { props: { modelValue: false, label: '启用' } })

    // find() rather than get(): this one asserts absence as well as presence.
    expect(wrapper.find('input[type="checkbox"]').exists()).toBe(true)
    // The input is present and transparent rather than replaced: screen
    // readers and the space bar both go through it.
    expect(wrapper.get('label').text()).toContain('启用')
  })

  it('reports the new value rather than mutating the prop', async () => {
    const wrapper = mount(AppCheckbox, { props: { modelValue: false } })

    await wrapper.get('input[type="checkbox"]').setValue(true)
    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual([true])
  })

  it('does not respond while disabled', async () => {
    const wrapper = mount(AppCheckbox, { props: { modelValue: false, disabled: true } })

    expect(wrapper.get('input[type="checkbox"]').attributes('disabled')).toBeDefined()
  })
})
