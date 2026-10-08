// [local] 生图冷却（openai:image_generation）的已知原因，与后端写入的 reason 保持一致。
const imageCooldownReasons = new Set([
  'openai_image_quota_exhausted',
  'openai_image_rate_limited',
  'openai_image_quota_pause',
  'openai_image_plan_limit',
  'openai_image_capability_lost',
  'openai_images_oauth_tool_unavailable',
  'openai_images_insufficient_balance'
])

export function isKnownImageCooldownReason(reason?: string | null): reason is string {
  return !!reason && imageCooldownReasons.has(reason)
}
