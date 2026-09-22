import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

/**
 * Routes and their navigation metadata.
 *
 * `label` is what the sidebar shows; the server localises stage names for the
 * same reason, so a view added after this file was written still displays
 * something a person can read.
 */
export interface NavMeta {
  label: string
  /** What the view is for, for a reader of this file. */
  description: string
  /** Shown in the sidebar. `false` for detail views reached from a list. */
  nav: boolean
}

export const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'dashboard',
    component: () => import('@/views/DashboardView.vue'),
    meta: {
      label: '总览',
      description: '项目统计、主机负载、模型与 AI Worker 状态。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/projects',
    name: 'projects',
    component: () => import('@/views/ProjectsView.vue'),
    meta: {
      label: '项目',
      description: '项目列表、状态筛选与删除。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/projects/new',
    name: 'project-create',
    component: () => import('@/views/CreateProjectView.vue'),
    meta: {
      label: '新建项目',
      description: '上传视频、选择语言对与翻译风格。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/projects/:id',
    name: 'project-detail',
    component: () => import('@/views/ProjectDetailView.vue'),
    meta: {
      label: '项目详情',
      description: 'Pipeline 各阶段状态、日志、重试与产物下载。',
      nav: false,
    } satisfies NavMeta,
  },
  {
    path: '/projects/:id/editor',
    name: 'subtitle-editor',
    component: () => import('@/views/SubtitleEditorView.vue'),
    meta: {
      label: '字幕编辑',
      description: '播放器、原文与译文对照、时间轴与 QC 面板。',
      nav: false,
    } satisfies NavMeta,
  },
  {
    path: '/projects/:id/review',
    name: 'review-queue',
    component: () => import('@/views/ReviewQueueView.vue'),
    meta: {
      label: '审校队列',
      description: '按类别分组的问题字幕，支持批量处理。',
      nav: false,
    } satisfies NavMeta,
  },
  {
    path: '/models',
    name: 'models',
    component: () => import('@/views/ModelsView.vue'),
    meta: {
      label: '模型',
      description: '模型清单、下载、删除与磁盘占用。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/providers',
    name: 'providers',
    component: () => import('@/views/ProvidersView.vue'),
    meta: {
      label: '翻译服务',
      description: 'OpenAI 兼容端点配置与连通性测试。密钥仅以掩码显示。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/settings',
    name: 'settings',
    component: () => import('@/views/SettingsView.vue'),
    meta: {
      label: '设置',
      description: '运行时设置，并显示每一项当前生效的来源。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/system',
    name: 'system',
    component: () => import('@/views/SystemView.vue'),
    meta: {
      label: '系统',
      description: '能力矩阵与 doctor 诊断结果。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/logs',
    name: 'logs',
    component: () => import('@/views/LogsView.vue'),
    meta: {
      label: '日志',
      description: '实时日志与历史检索。',
      nav: true,
    } satisfies NavMeta,
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    redirect: '/',
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

/** Navigation entries, in sidebar order. */
export const navItems = routes
  .filter((route) => (route.meta as NavMeta | undefined)?.nav === true)
  .map((route) => ({
    name: String(route.name),
    path: route.path,
    label: (route.meta as unknown as NavMeta).label,
  }))
