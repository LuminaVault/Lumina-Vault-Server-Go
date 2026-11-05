-- KnowledgeGraphService. Enum filters arrive already narrowed to known values;
-- an empty kinds or predicates array means no filter, as Swift omits the clause.

-- name: UserTier :one
SELECT tier FROM users WHERE id = $1;

-- name: GraphNodes :many
SELECT id, kind, label, summary, occurred_at, confidence
FROM knowledge_nodes
WHERE tenant_id = @tenant_id
  AND confidence >= @minimum_confidence::double precision
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY(@kinds::text[]))
ORDER BY updated_at DESC, id ASC
LIMIT @row_limit;

-- name: GraphEdges :many
SELECT id, from_node_id, to_node_id, predicate, state, confidence, rationale, counter_evidence
FROM knowledge_edges
WHERE tenant_id = @tenant_id
  AND from_node_id = ANY(@node_ids::uuid[])
  AND to_node_id = ANY(@node_ids::uuid[])
  AND state = ANY(@states::text[])
  AND confidence >= @minimum_confidence::double precision
  AND (cardinality(@predicates::text[]) = 0 OR predicate = ANY(@predicates::text[]))
ORDER BY confidence DESC, id ASC
LIMIT @row_limit;

-- name: Evidence :many
SELECT id, node_id, edge_id, memory_id, source_vault_file_id, quote, start_offset, end_offset
FROM knowledge_evidence
WHERE tenant_id = @tenant_id
  AND (node_id = ANY(@node_ids::uuid[]) OR edge_id = ANY(@edge_ids::uuid[]))
ORDER BY created_at ASC, id ASC;

-- name: ConnectionPaths :many
WITH RECURSIVE paths(current_id, node_ids, edge_ids, path_confidence, depth) AS (
    SELECT @from_node::uuid, ARRAY[@from_node::uuid], ARRAY[]::uuid[], 1.0::double precision, 0
    UNION ALL
    SELECT
        CASE WHEN e.from_node_id = p.current_id THEN e.to_node_id ELSE e.from_node_id END,
        p.node_ids || CASE WHEN e.from_node_id = p.current_id THEN e.to_node_id ELSE e.from_node_id END,
        p.edge_ids || e.id,
        p.path_confidence * e.confidence,
        p.depth + 1
    FROM paths p
    JOIN knowledge_edges e
      ON e.tenant_id = @tenant_id
     AND (e.from_node_id = p.current_id OR e.to_node_id = p.current_id)
     AND e.state IN ('asserted', 'suggested', 'confirmed')
    WHERE p.depth < @max_depth::integer
      AND NOT (CASE WHEN e.from_node_id = p.current_id THEN e.to_node_id ELSE e.from_node_id END = ANY(p.node_ids))
)
SELECT node_ids::uuid[] AS node_ids, edge_ids::uuid[] AS edge_ids, path_confidence::double precision AS path_confidence
FROM paths
WHERE current_id = @to_node::uuid
ORDER BY path_confidence DESC, array_length(edge_ids, 1) ASC
LIMIT 5;

-- name: NodesByID :many
SELECT id, kind, label, summary, occurred_at, confidence
FROM knowledge_nodes
WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]);

-- name: EdgesByID :many
SELECT id, from_node_id, to_node_id, predicate, state, confidence, rationale, counter_evidence
FROM knowledge_edges
WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]);

-- name: ReviewEdge :one
UPDATE knowledge_edges
SET state = @state, review_note = @note, reviewed_at = NOW(), updated_at = NOW()
WHERE tenant_id = @tenant_id AND id = @id AND state = 'suggested'
RETURNING id, from_node_id, to_node_id, predicate, state, confidence, rationale, counter_evidence;
