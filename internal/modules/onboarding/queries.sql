-- Onboarding (Swift: Sources/App/Onboarding/OnboardingController.swift, model OnboardingState).

-- name: GetOnboardingState :one
SELECT id, tenant_id, signup_completed, signup_completed_at, email_verified_completed, email_verified_completed_at,
       soul_configured_completed, soul_configured_completed_at, first_capture_completed, first_capture_completed_at,
       first_kb_compile_completed, first_kb_compile_completed_at, first_query_completed, first_query_completed_at,
       first_memory_compile_completed, first_memory_compile_completed_at,
       brain_configured_completed, brain_configured_completed_at, guided_start_dismissed_at
FROM onboarding_state WHERE tenant_id = $1 LIMIT 1;

-- name: InsertOnboardingState :exec
-- OnboardingState(tenantID:signupCompleted: true).save(): every field the
-- initializer sets, plus both Fluent timestamps. The other *_at stay NULL.
INSERT INTO onboarding_state (id, tenant_id, signup_completed, signup_completed_at, email_verified_completed,
  soul_configured_completed, first_capture_completed, first_kb_compile_completed, first_memory_compile_completed,
  first_query_completed, brain_configured_completed, guided_start_dismissed_at, created_at, updated_at)
VALUES ($1, $2, true, $3, false, false, false, false, false, false, false, NULL, $4, $4);
