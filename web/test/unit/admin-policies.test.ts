import { describe, expect, it } from 'vitest'
import {
  ADMIN_ACTIONS,
  ADMIN_RESOURCE_FORMS,
  adminArn,
  adminConditionKeys,
  adminRequestResource,
  adminResourceType,
  IAM_POLICY_TEMPLATES,
  isAdminAction,
  parseConditionLines,
  parsePolicyText,
  renderIamTemplate,
  SIMULATOR_ACTIONS,
  templateParamErrors,
  validateAdminResource,
  validateCidrList,
  validatePolicyDocument,
  validatePolicyName,
  validateUserName
} from '~/components/admin/-policies'
import { applyBucketTemplate, BUCKET_POLICY_TEMPLATES, bucketPolicyTemplate } from '~/components/bucket-settings/-policy-templates'
import { credentialsFileSnippet, envSnippet, profileName } from '~/components/admin/-credentials'

describe('name validation', () => {
  it('validates user names like the server', () => {
    expect(validateUserName('alice')).toBeUndefined()
    expect(validateUserName('a.b+c=d,e@f_g-h')).toBeUndefined()
    expect(validateUserName('')).toBeDefined()
    expect(validateUserName('with space')).toBeDefined()
    expect(validateUserName('x'.repeat(65))).toBeDefined()
    expect(validateUserName('Root')).toMatch(/reserved/)
    expect(validateUserName('anonymous')).toMatch(/reserved/)
  })

  it('validates policy names and rejects built-in names', () => {
    expect(validatePolicyName('my-policy')).toBeUndefined()
    expect(validatePolicyName('x'.repeat(129))).toBeDefined()
    expect(validatePolicyName('readonly', ['readonly'])).toMatch(/built-in/)
    expect(validatePolicyName('a/b')).toBeDefined()
  })
})

describe('IAM policy templates', () => {
  for (const t of IAM_POLICY_TEMPLATES) {
    it(`${t.id} renders a valid identity policy with the bucket substituted`, () => {
      const text = renderIamTemplate(t.id, { bucket: 'photos', cidr: '10.0.0.0/8' })
      const parsed = JSON.parse(text) as unknown
      expect(validatePolicyDocument(parsed, 'identity')).toBeUndefined()
      if (t.params.includes('bucket')) {
        expect(text).toContain('arn:aws:s3:::photos')
        expect(text).not.toContain('arn:aws:s3:::*"')
      }
    })
  }

  it('prefix-per-user uses ${aws:username}', () => {
    const text = renderIamTemplate('prefix-per-user', { bucket: 'home' })
    expect(text).toContain('arn:aws:s3:::home/${aws:username}/*')
    expect(text).toContain('"s3:prefix"')
  })

  it('ip-restricted denies outside the given networks', () => {
    const doc = JSON.parse(renderIamTemplate('ip-restricted', { cidr: '10.0.0.0/8, 192.168.1.0/24' })) as { Statement: { Effect: string, Condition: Record<string, Record<string, string[]>> }[] }
    expect(doc.Statement[0]!.Effect).toBe('Deny')
    expect(doc.Statement[0]!.Condition.NotIpAddress!['aws:SourceIp']).toEqual(['10.0.0.0/8', '192.168.1.0/24'])
  })

  it('deny-delete without a bucket applies to all buckets', () => {
    expect(renderIamTemplate('deny-delete', {})).toContain('arn:aws:s3:::*/*')
  })

  it('reports missing / invalid template parameters', () => {
    const rw = IAM_POLICY_TEMPLATES.find(t => t.id === 'readwrite-bucket')!
    expect(templateParamErrors(rw, {})).toEqual({ bucket: 'Required' })
    expect(templateParamErrors(rw, { bucket: 'Bad Name' }).bucket).toBeDefined()
    const ip = IAM_POLICY_TEMPLATES.find(t => t.id === 'ip-restricted')!
    expect(templateParamErrors(ip, { cidr: '10.0.0.0/33' }).cidr).toBeDefined()
    expect(templateParamErrors(ip, { cidr: '10.0.0.0/8' })).toEqual({})
  })

  it('validates CIDR lists', () => {
    expect(validateCidrList('10.0.0.0/8')).toBeUndefined()
    expect(validateCidrList('192.168.1.10, fd00::/8')).toBeUndefined()
    expect(validateCidrList('300.1.1.1/8')).toBeDefined()
    expect(validateCidrList('')).toBeDefined()
  })
})

describe('admin actions, resources and condition keys', () => {
  it('lists every admin action, including the new self-service and probe actions', () => {
    for (const a of ['admin:ProbeNetwork', 'admin:CreateOwnAccessKey', 'admin:DeleteOwnAccessKey', 'admin:GetDiscovery', 'admin:ScanNetwork', 'admin:UpdateUser', 'admin:GetPolicy', 'admin:GetBucket']) {
      expect(ADMIN_ACTIONS).toContain(a)
      expect(SIMULATOR_ACTIONS).toContain(a)
    }
    expect(new Set(SIMULATOR_ACTIONS).size).toBe(SIMULATOR_ACTIONS.length)
    for (const a of ADMIN_ACTIONS) expect(validatePolicyDocument({ Statement: [{ Effect: 'Allow', Action: a, Resource: '*' }] }, 'identity')).toBeUndefined()
  })

  it('maps admin actions to their per-target resource type', () => {
    for (const a of ['admin:CreateUser', 'admin:GetUser', 'admin:UpdateUser', 'admin:DeleteUser', 'admin:CreateAccessKey', 'admin:UpdateAccessKey', 'admin:DeleteAccessKey']) expect(adminResourceType(a)).toBe('user')
    for (const a of ['admin:GetPolicy', 'admin:PutPolicy', 'admin:DeletePolicy']) expect(adminResourceType(a)).toBe('policy')
    for (const a of ['admin:GetBucket', 'admin:ForceDeleteBucket']) expect(adminResourceType(a)).toBe('bucket')
    for (const a of ['admin:ListUsers', 'admin:ProbeNetwork', 'admin:CreateOwnAccessKey', 'admin:UpdateConfig', 's3:GetObject']) expect(adminResourceType(a)).toBeUndefined()
    expect(adminResourceType(' Admin:updateuser ')).toBe('user')
    expect(isAdminAction('admin:GetStats')).toBe(true)
    expect(isAdminAction('s3:GetObject')).toBe(false)
  })

  it('offers the three admin ARN forms, which are valid identity policy resources', () => {
    const arns = ADMIN_RESOURCE_FORMS.map(f => f.arn)
    expect(arns).toEqual(['arn:gos3:admin:::*', 'arn:gos3:admin:::user/<name>', 'arn:gos3:admin:::policy/<name>', 'arn:gos3:admin:::bucket/<name>'])
    for (const t of ['user', 'policy', 'bucket'] as const) {
      expect(validatePolicyDocument({ Statement: [{ Effect: 'Allow', Action: 'admin:*', Resource: adminArn(t, 'team-*') }] }, 'identity')).toBeUndefined()
    }
  })

  it('validates and normalises simulator admin resources', () => {
    expect(validateAdminResource('')).toBeUndefined()
    expect(validateAdminResource('arn:gos3:admin:::*')).toBeUndefined()
    expect(validateAdminResource('arn:gos3:admin:::user/alice')).toBeUndefined()
    expect(validateAdminResource('arn:gos3:admin:::policy/readonly')).toBeUndefined()
    expect(validateAdminResource('arn:gos3:admin:::bucket/photos')).toBeUndefined()
    expect(validateAdminResource('arn:gos3:admin:::user/')).toBeDefined()
    expect(validateAdminResource('arn:gos3:admin:::thing/x')).toBeDefined()
    expect(validateAdminResource('arn:aws:s3:::b')).toBeDefined()
    expect(adminRequestResource('  ')).toBeUndefined()
    expect(adminRequestResource('*')).toBeUndefined()
    expect(adminRequestResource('arn:gos3:admin:::*')).toBeUndefined()
    expect(adminRequestResource(' arn:gos3:admin:::user/a ')).toBe('arn:gos3:admin:::user/a')
  })

  it('suggests admin:PolicyName for CreateUser and UpdateUser only', () => {
    expect(adminConditionKeys('admin:CreateUser')).toEqual(['admin:PolicyName'])
    expect(adminConditionKeys('admin:UpdateUser')).toEqual(['admin:PolicyName'])
    expect(adminConditionKeys('admin:DeleteUser')).toEqual([])
    expect(adminConditionKeys('s3:GetObject')).toEqual([])
  })

  it('own-access-keys allows creating and deleting own keys at service level', () => {
    const d = JSON.parse(renderIamTemplate('own-access-keys', {})) as { Statement: { Effect: string, Action: string[], Resource: string[] }[] }
    expect(d.Statement).toEqual([{ Sid: 'OwnAccessKeys', Effect: 'Allow', Action: ['admin:CreateOwnAccessKey', 'admin:DeleteOwnAccessKey'], Resource: ['arn:gos3:admin:::*'] }])
  })

  it('user-manager scopes user actions to a pattern and limits attachable policies', () => {
    type Doc = { Statement: { Sid: string, Action: string[], Resource: string[], Condition?: Record<string, Record<string, unknown>> }[] }
    const d = JSON.parse(renderIamTemplate('user-manager', { users: 'team-*', policies: 'readonly, readwrite' })) as Doc
    const manage = d.Statement.find(s => s.Sid === 'ManageUsers')!
    expect(manage.Resource).toEqual(['arn:gos3:admin:::user/team-*'])
    expect(manage.Action).toContain('admin:UpdateUser')
    expect(manage.Condition).toEqual({ 'ForAllValues:StringEquals': { 'admin:PolicyName': ['readonly', 'readwrite'] } })
    const none = JSON.parse(renderIamTemplate('user-manager', { users: 'team-*' })) as Doc
    expect(none.Statement.find(s => s.Sid === 'ManageUsers')!.Condition).toEqual({ Null: { 'admin:PolicyName': 'true' } })
    const t = IAM_POLICY_TEMPLATES.find(x => x.id === 'user-manager')!
    expect(templateParamErrors(t, {})).toEqual({ users: 'Required' })
    expect(templateParamErrors(t, { users: 'bad name' }).users).toBeDefined()
    expect(templateParamErrors(t, { users: 'team-*', policies: 'ok, a/b' }).policies).toBeDefined()
    expect(templateParamErrors(t, { users: 'team-*', policies: 'readonly' })).toEqual({})
  })
})

describe('policy document validation', () => {
  const base = { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: 's3:GetObject', Resource: 'arn:aws:s3:::b/*' }] }

  it('accepts a minimal identity policy and a single statement object', () => {
    expect(validatePolicyDocument(base, 'identity')).toBeUndefined()
    expect(validatePolicyDocument({ Statement: base.Statement[0] }, 'identity')).toBeUndefined()
  })

  it('rejects structural problems', () => {
    expect(validatePolicyDocument([], 'identity')).toBeDefined()
    expect(validatePolicyDocument({ Version: '2020-01-01', Statement: base.Statement }, 'identity')).toMatch(/Version/)
    expect(validatePolicyDocument({ Statement: [] }, 'identity')).toMatch(/Statement/)
    expect(validatePolicyDocument({ Statement: [{ Effect: 'Maybe', Action: '*', Resource: '*' }] }, 'identity')).toMatch(/Effect/)
    expect(validatePolicyDocument({ Statement: [{ Effect: 'Allow', Resource: '*' }] }, 'identity')).toMatch(/Action/)
    expect(validatePolicyDocument({ Statement: [{ Effect: 'Allow', Action: '*' }] }, 'identity')).toMatch(/Resource/)
    expect(validatePolicyDocument({ Statement: [{ Effect: 'Allow', Action: 'ec2:Run', Resource: '*' }] }, 'identity')).toMatch(/invalid action/)
    expect(validatePolicyDocument({ Statement: [{ Sid: 'a', Effect: 'Allow', Action: '*', Resource: '*' }, { Sid: 'a', Effect: 'Allow', Action: '*', Resource: '*' }] }, 'identity')).toMatch(/duplicate Sid/)
  })

  it('enforces identity vs bucket policy rules', () => {
    const withPrincipal = { Statement: [{ ...base.Statement[0], Principal: '*' }] }
    expect(validatePolicyDocument(withPrincipal, 'identity')).toMatch(/Principal/)
    expect(validatePolicyDocument(withPrincipal, 'bucket', 'b')).toBeUndefined()
    expect(validatePolicyDocument(base, 'bucket', 'b')).toMatch(/Principal/)
    expect(validatePolicyDocument(withPrincipal, 'bucket', 'other')).toMatch(/does not belong/)
    expect(validatePolicyDocument({ Statement: [{ Effect: 'Allow', Principal: '*', Action: 'admin:GetStats', Resource: 'arn:aws:s3:::b' }] }, 'bucket', 'b')).toMatch(/admin/)
  })

  it('parsePolicyText reports JSON and validation errors', () => {
    expect(parsePolicyText('{', 'identity').error).toBeDefined()
    expect(parsePolicyText(JSON.stringify(base), 'identity').doc).toEqual(base)
  })
})

describe('bucket policy templates', () => {
  for (const t of BUCKET_POLICY_TEMPLATES) {
    it(`${t.id} produces a valid bucket policy for the bucket`, () => {
      const doc = bucketPolicyTemplate(t.id, 'photos', { user: 'alice' })
      const text = JSON.stringify(doc)
      expect(validatePolicyDocument(JSON.parse(text), 'bucket', 'photos')).toBeUndefined()
      expect(text).toContain('arn:aws:s3:::photos')
      if (t.needsUser) expect(text).toContain('user/alice')
    })
  }

  it('replaces invalid text and appends to an existing policy, replacing equal Sids', () => {
    const first = applyBucketTemplate('', 'public-read', 'b')
    expect(JSON.parse(first).Statement).toHaveLength(1)
    const both = applyBucketTemplate(first, 'deny-insecure-transport', 'b')
    const sids = (JSON.parse(both) as { Statement: { Sid: string }[] }).Statement.map(s => s.Sid)
    expect(sids).toEqual(['PublicRead', 'DenyInsecureTransport'])
    const again = applyBucketTemplate(both, 'public-read', 'b')
    expect((JSON.parse(again) as { Statement: unknown[] }).Statement).toHaveLength(2)
    expect(validatePolicyDocument(JSON.parse(again), 'bucket', 'b')).toBeUndefined()
  })
})

describe('simulator conditions', () => {
  it('parses key=value lines', () => {
    expect(parseConditionLines('aws:SourceIp=10.0.0.1\n\ns3:prefix = a/, b/')).toEqual({
      conditions: { 'aws:SourceIp': ['10.0.0.1'], 's3:prefix': ['a/', 'b/'] }
    })
    expect(parseConditionLines('oops').error).toMatch(/Line 1/)
  })
})

describe('credential snippets', () => {
  const input = { accessKey: 'AKIAEXAMPLE', secretKey: 'secret/+=', endpoint: 'http://10.0.0.5:9000', region: 'eu-1', profile: 'my profile!' }

  it('renders a .env snippet', () => {
    expect(envSnippet(input)).toBe('AWS_ACCESS_KEY_ID=AKIAEXAMPLE\nAWS_SECRET_ACCESS_KEY=secret/+=\nAWS_REGION=eu-1\nAWS_ENDPOINT_URL=http://10.0.0.5:9000\n')
  })

  it('renders a credentials/config file snippet with a sanitised profile', () => {
    const s = credentialsFileSnippet(input)
    expect(s).toContain('[my-profile]')
    expect(s).toContain('[profile my-profile]')
    expect(s).toContain('aws_secret_access_key = secret/+=')
    expect(s).toContain('endpoint_url = http://10.0.0.5:9000')
    expect(profileName('')).toBe('gos3')
  })
})
