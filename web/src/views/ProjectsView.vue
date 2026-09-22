<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'

import { api, type ListProjectsQuery } from '@/api/client'
import { formatDuration, useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'
import type { Project } from '@/types/api'

const events = useEventStore()

const filter = ref<'active' | 'archived' | 'all'>('active')
const search = ref('')

const projects = useAsync(() => api.projects.list(query()))

function query(): ListProjectsQuery {
  const value: ListProjectsQuery = { status: filter.value, limit: 100 }
  if (search.value.trim() !== '') value.q = search.value.trim()
  return value
}

onMounted(projects.run)

// Refetched when a project changes anywhere — including from another tab, or
// from a run finishing. The list is the thing a user leaves open.
watch(() => events.resyncCount, projects.run)

let searchTimer: ReturnType<typeof setTimeout> | null = null
watch(search, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(projects.run, 250)
})

function stageLabel(project: Project): string {
  if (project.current_job) {
    return `${project.current_job.status} · ${(project.current_job.progress * 100).toFixed(0)}%`
  }
  if (project.segment_count === 0) return '尚未运行'
  return `${project.segment_count} 行`
}

const actionError = ref<string | null>(null)

async function remove(project: Project): Promise<void> {
  actionError.value = null
  // Two deliberate steps: the browser's own modal is a real interruption, and
  // the API refuses an unconfirmed delete anyway. Deleting a project's files is
  // unrecoverable and the confirmation is the only thing standing in front of it.
  if (!confirm(`删除项目「${project.name}」？文件会保留。`)) return

  try {
    await api.projects.remove(project.id, { confirm: true, deleteFiles: false })
    await projects.run()
  } catch (cause) {
    actionError.value = cause instanceof Error ? cause.message : String(cause)
  }
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center gap-3">
      <div class="flex rounded border border-line bg-surface-raised">
        <button
          v-for="option in (['active', 'archived', 'all'] as const)"
          :key="option"
          class="px-3 py-1.5 text-sm transition"
          :class="filter === option ? 'bg-accent text-accent-ink' : 'text-ink-muted hover:text-ink'"
          @click="filter = option"
        >
          {{ { active: '进行中', archived: '已归档', all: '全部' }[option] }}
        </button>
      </div>

      <input
        v-model="search"
        type="search"
        placeholder="搜索项目名"
        class="min-w-48 flex-1 rounded border border-line bg-surface-raised px-3 py-1.5 text-sm outline-none placeholder:text-ink-faint focus:border-accent"
      />

      <RouterLink
        to="/projects/new"
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink transition hover:opacity-90"
      >
        新建项目
      </RouterLink>
    </div>

    <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ actionError }}
    </p>

    <p v-if="projects.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ projects.error.value }}
      <button class="ml-2 text-accent hover:underline" @click="projects.run">重试</button>
    </p>

    <div v-else-if="projects.loading.value && !projects.data.value" class="p-6 text-center text-sm text-ink-muted">
      加载中…
    </div>

    <div
      v-else-if="projects.data.value && projects.data.value.items.length === 0"
      class="rounded border border-dashed border-line p-10 text-center text-sm text-ink-muted"
    >
      还没有项目。
      <RouterLink to="/projects/new" class="text-accent hover:underline">创建一个</RouterLink>
    </div>

    <table v-else-if="projects.data.value" class="w-full border-collapse text-sm">
      <thead>
        <tr class="border-b border-line text-left text-xs text-ink-faint">
          <th class="py-2 pr-4 font-medium">名称</th>
          <th class="py-2 pr-4 font-medium">语言对</th>
          <th class="py-2 pr-4 font-medium">风格</th>
          <th class="py-2 pr-4 font-medium">时长</th>
          <th class="py-2 pr-4 font-medium">状态</th>
          <th class="py-2 font-medium"></th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="project in projects.data.value.items"
          :key="project.id"
          class="border-b border-line/60 transition hover:bg-surface-raised"
        >
          <td class="py-2 pr-4">
            <RouterLink :to="`/projects/${project.id}`" class="hover:text-accent">
              {{ project.name }}
            </RouterLink>
          </td>
          <td class="py-2 pr-4 text-ink-muted">
            {{ project.source_language }} → {{ project.target_language }}
          </td>
          <td class="py-2 pr-4 text-ink-muted">{{ project.style }}</td>
          <td class="py-2 pr-4 tabular-nums text-ink-muted">{{ formatDuration(project.duration) }}</td>
          <td class="py-2 pr-4">
            <span class="text-ink-muted">{{ stageLabel(project) }}</span>
            <!--
              The review count is shown only when it is non-zero. A permanent
              "0 待审校" column trains people to stop reading it.
            -->
            <span v-if="project.needs_review_count > 0" class="ml-2 text-status-running">
              {{ project.needs_review_count }} 待审校
            </span>
          </td>
          <td class="py-2 text-right">
            <button class="text-xs text-ink-faint transition hover:text-status-failed" @click="remove(project)">
              删除
            </button>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
