<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'

/**
 * A dropdown.
 *
 * Built by hand because a native `<select>` cannot be styled where it matters:
 * the popup is drawn by the operating system, outside the page, so no amount
 * of CSS reaches the list itself. Every other control here keeps its native
 * element and changes only its appearance; this is the one that has to be
 * rebuilt.
 *
 * Rebuilt properly, which means the parts a naive replacement drops: arrow
 * keys that move, a highlight that follows the keyboard, Enter and Escape,
 * typing to jump to an option, a click anywhere else to dismiss, and the list
 * flipping upward when there is no room below it. It is taken out of the page
 * with Teleport and positioned against the trigger, because an absolutely
 * positioned popup inside a scrolling panel is clipped by it.
 */
const props = withDefaults(
  defineProps<{
    options: { value: string; label: string }[]
    disabled?: boolean
    placeholder?: string
    id?: string
    ariaLabel?: string
  }>(),
  { disabled: false, placeholder: '请选择', id: undefined, ariaLabel: undefined },
)

const model = defineModel<string>({ default: '' })

const open = ref(false)
const activeIndex = ref(-1)
const triggerRef = ref<HTMLButtonElement | null>(null)
const listRef = ref<HTMLElement | null>(null)

/** Where the popup is drawn, in viewport coordinates. */
const position = ref({ top: 0, left: 0, width: 0, flip: false })

const selectedIndex = computed(() =>
  props.options.findIndex((option) => option.value === model.value),
)

const selectedLabel = computed(
  () => props.options.find((option) => option.value === model.value)?.label ?? '',
)

/** Typed characters, accumulated so that "la" matches a word starting with it. */
let typing = ''
let typingTimer: number | undefined

function measure(): void {
  const trigger = triggerRef.value
  if (trigger === null) return

  const rect = trigger.getBoundingClientRect()
  const below = window.innerHeight - rect.bottom
  const needed = Math.min(props.options.length * 36 + 12, 280)

  // Flipped up when the list would run off the bottom. Measured rather than
  // guessed, because the trigger sits near the bottom of the window on some
  // pages and near the top on others.
  const flip = below < needed && rect.top > below

  position.value = { top: flip ? rect.top - 4 : rect.bottom + 4, left: rect.left, width: rect.width, flip }
}

function openList(): void {
  if (props.disabled || open.value) return

  measure()
  open.value = true
  activeIndex.value = selectedIndex.value >= 0 ? selectedIndex.value : 0

  // Listeners are added only while open, and removed on close: a page with
  // forty dropdowns should not have forty scroll handlers.
  window.addEventListener('scroll', measure, true)
  window.addEventListener('resize', measure)
  document.addEventListener('pointerdown', onPointerDown, true)

  void nextTick(() => listRef.value?.focus())
}

function closeList(): void {
  if (!open.value) return
  open.value = false

  window.removeEventListener('scroll', measure, true)
  window.removeEventListener('resize', measure)
  document.removeEventListener('pointerdown', onPointerDown, true)

  triggerRef.value?.focus()
}

function onPointerDown(event: PointerEvent): void {
  const target = event.target as Node
  if (triggerRef.value?.contains(target) || listRef.value?.contains(target)) return
  closeList()
}

function choose(index: number): void {
  const option = props.options[index]
  if (option === undefined) return

  model.value = option.value
  closeList()
}

function move(delta: number): void {
  if (props.options.length === 0) return

  const next = activeIndex.value + delta
  // Wraps, like a native select does.
  activeIndex.value = (next + props.options.length) % props.options.length
  scrollActiveIntoView()
}

function scrollActiveIntoView(): void {
  void nextTick(() => {
    listRef.value
      ?.querySelector(`[data-index="${activeIndex.value}"]`)
      ?.scrollIntoView({ block: 'nearest' })
  })
}

function onKeydown(event: KeyboardEvent): void {
  if (props.disabled) return

  if (!open.value) {
    // A native select opens on the arrow keys as well as on Enter, which is
    // how someone who is not using a mouse reaches it.
    if (['Enter', ' ', 'ArrowDown', 'ArrowUp'].includes(event.key)) {
      event.preventDefault()
      openList()
    }
    return
  }

  switch (event.key) {
    case 'ArrowDown':
      event.preventDefault()
      move(1)
      break
    case 'ArrowUp':
      event.preventDefault()
      move(-1)
      break
    case 'Home':
      event.preventDefault()
      activeIndex.value = 0
      scrollActiveIntoView()
      break
    case 'End':
      event.preventDefault()
      activeIndex.value = props.options.length - 1
      scrollActiveIntoView()
      break
    case 'Enter':
    case ' ':
      event.preventDefault()
      choose(activeIndex.value)
      break
    case 'Escape':
      event.preventDefault()
      closeList()
      break
    case 'Tab':
      closeList()
      break
    default:
      if (event.key.length === 1) typeAhead(event.key)
  }
}

/** Jumps to the next option starting with what has been typed. */
function typeAhead(character: string): void {
  typing += character.toLowerCase()
  window.clearTimeout(typingTimer)
  typingTimer = window.setTimeout(() => (typing = ''), 600)

  const start = activeIndex.value + 1
  for (let offset = 0; offset < props.options.length; offset++) {
    const index = (start + offset) % props.options.length
    if (props.options[index]?.label.toLowerCase().startsWith(typing)) {
      activeIndex.value = index
      scrollActiveIntoView()
      return
    }
  }
}

watch(
  () => props.disabled,
  (disabled) => {
    if (disabled) closeList()
  },
)

onBeforeUnmount(closeList)
</script>

<template>
  <div class="relative">
    <button
      :id="id"
      ref="triggerRef"
      type="button"
      role="combobox"
      :aria-expanded="open"
      :aria-controls="id ? `${id}-listbox` : undefined"
      :aria-label="ariaLabel"
      :aria-activedescendant="open && id && activeIndex >= 0 ? `${id}-option-${activeIndex}` : undefined"
      :disabled="disabled"
      aria-haspopup="listbox"
      class="flex w-full items-center justify-between gap-2 rounded border border-line bg-surface px-2 py-1.5 text-left text-sm text-ink outline-none transition hover:border-ink-faint focus-visible:border-accent focus-visible:ring-1 focus-visible:ring-accent disabled:cursor-not-allowed disabled:opacity-50"
      @click="open ? closeList() : openList()"
      @keydown="onKeydown"
    >
      <span class="truncate" :class="selectedLabel === '' ? 'text-ink-faint' : ''">
        {{ selectedLabel === '' ? placeholder : selectedLabel }}
      </span>

      <svg
        viewBox="0 0 12 12"
        class="size-3 shrink-0 text-ink-faint transition-transform"
        :class="open ? 'rotate-180' : ''"
        aria-hidden="true"
      >
        <path
          d="M2.5 4.5 L6 8 L9.5 4.5"
          fill="none"
          stroke="currentColor"
          stroke-width="1.6"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
    </button>

    <Teleport to="body">
      <div
        v-if="open"
        :id="id ? `${id}-listbox` : undefined"
        ref="listRef"
        role="listbox"
        tabindex="-1"
        class="fixed z-50 max-h-70 overflow-y-auto rounded border border-line bg-surface-raised py-1 shadow-lg outline-none"
        :style="{
          top: `${position.top}px`,
          left: `${position.left}px`,
          width: `${position.width}px`,
          transform: position.flip ? 'translateY(-100%)' : 'none',
        }"
        @keydown="onKeydown"
      >
        <div
          v-for="(option, index) in options"
          :id="id ? `${id}-option-${index}` : undefined"
          :key="option.value"
          :data-index="index"
          role="option"
          :aria-selected="option.value === model"
          class="cursor-pointer px-3 py-1.5 text-sm transition"
          :class="[
            index === activeIndex ? 'bg-surface text-ink' : 'text-ink-muted',
            option.value === model ? 'font-medium' : '',
          ]"
          @pointerenter="activeIndex = index"
          @click="choose(index)"
        >
          {{ option.label }}
        </div>
      </div>
    </Teleport>
  </div>
</template>
