import { request } from './client'
import type { OrganizationWithRole } from './types'

export async function createOrganization(
  name: string,
  slug: string,
): Promise<OrganizationWithRole> {
  const body = await request<{ organization: OrganizationWithRole }>('/organizations', {
    method: 'POST',
    body: { name, slug },
  })
  return body.organization
}
