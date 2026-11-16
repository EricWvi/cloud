import { beforeEach, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { Role } from '@/lib/roles-api/generated'
import { installMarketplaceBackend, renderSettingsPage } from '../test-utils'
import RolesPage from './page'

const serverRole = (overrides: Partial<Role> = {}): Role => ({
  id: '00000000-0000-4000-8000-000000000901',
  name: '代码审查',
  description: '检查改动的正确性和测试覆盖。',
  content: '# 代码审查\n\n先看行为，再看实现。',
  version: 1,
  createdAt: '2026-11-02T08:00:00Z',
  updatedAt: '2026-11-02T08:00:00Z',
  ...overrides,
})

const roleFile = (name: string, description = '导入的角色', body = '# 导入角色') =>
  new File([`---\nname: ${name}\ndescription: ${description}\n---\n\n${body}`], 'role.md', {
    type: 'text/markdown',
  })

beforeEach(() => localStorage.clear())

it('creates, renders, edits and deletes a server-backed role', async () => {
  const backend = installMarketplaceBackend({ roles: [serverRole()] })
  const user = userEvent.setup()
  renderSettingsPage(<RolesPage />)
  expect(await screen.findByRole('article', { name: '代码审查' })).toBeVisible()
  // The role says where it lives: no browser-local catalog any more.
  expect(backend.seen).toContain(`GET /api/v2/tenants/00000000-0000-4000-8000-0000000000aa/roles`)

  await user.click(screen.getByRole('button', { name: '新建角色' }))
  await user.type(screen.getByLabelText('标题'), '发布检查')
  await user.type(screen.getByLabelText('描述'), '发布前核对版本和说明')
  await user.type(screen.getByLabelText('内容（Markdown）'), '# 发布检查\n\n核对版本。')
  await user.click(screen.getByRole('button', { name: '保存角色' }))
  const row = await screen.findByRole('article', { name: '发布检查' })
  await user.click(within(row).getByRole('button', { name: '查看角色 发布检查' }))
  expect(await screen.findByText('核对版本。')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: '编辑角色' }))
  await user.clear(screen.getByLabelText('描述'))
  await user.type(screen.getByLabelText('描述'), '已更新')
  await user.click(screen.getByRole('button', { name: '保存角色' }))
  expect(
    within(await screen.findByRole('dialog')).getByText('已更新'),
  ).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: '删除角色' }))
  await user.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: '删除角色',
    }),
  )
  await waitFor(() =>
    expect(screen.queryByRole('article', { name: '发布检查' })).not.toBeInTheDocument(),
  )
  // Persistence belongs to the server, not to this browser's storage.
  await user.click(screen.getByRole('button', { name: '导入角色' }))
  expect(await screen.findByRole('dialog')).toBeInTheDocument()
  expect(Object.keys(localStorage)).toHaveLength(0)
})

it('imports a role Markdown file and resolves a same-name conflict on the server', async () => {
  const backend = installMarketplaceBackend({ roles: [serverRole()] })
  const user = userEvent.setup()
  renderSettingsPage(<RolesPage />)
  await screen.findByRole('article', { name: '代码审查' })

  await user.click(screen.getByRole('button', { name: '导入角色' }))
  await user.upload(screen.getByLabelText('角色 Markdown 文件'), roleFile('发布检查'))
  await user.click(screen.getByRole('button', { name: '导入' }))
  await waitFor(() =>
    expect(backend.seen).toContain(
      `POST /api/v2/tenants/00000000-0000-4000-8000-0000000000aa/roles/import`,
    ),
  )
  await waitFor(() =>
    expect(screen.getByRole('article', { name: '发布检查' })).toBeVisible(),
  )

  await user.click(screen.getByRole('button', { name: '导入角色' }))
  await user.upload(
    screen.getByLabelText('角色 Markdown 文件'),
    roleFile('代码审查', '覆盖后的描述', '覆盖后的内容'),
  )
  await user.click(screen.getByRole('button', { name: '导入' }))
  expect(await screen.findByText('已有同名角色')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: '覆盖并导入' }))
  await waitFor(() =>
    expect(screen.getByRole('article', { name: '代码审查' })).toHaveTextContent(
      '覆盖后的描述',
    ),
  )
})
