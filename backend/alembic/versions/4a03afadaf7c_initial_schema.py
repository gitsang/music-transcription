"""initial schema"""
from alembic import op
import sqlalchemy as sa
revision = '4a03afadaf7c'
down_revision = None
branch_labels = None
depends_on = None

def upgrade():
    op.create_table('audit_logs',
    sa.Column('id', sa.String(length=36), nullable=False),
    sa.Column('actor_id', sa.String(length=36), nullable=True),
    sa.Column('action', sa.String(length=64), nullable=False),
    sa.Column('resource_id', sa.String(length=64), nullable=True),
    sa.Column('details', sa.JSON(), nullable=False),
    sa.Column('created_at', sa.DateTime(), nullable=False),
    sa.PrimaryKeyConstraint('id')
    )
    op.create_index(op.f('ix_audit_logs_actor_id'), 'audit_logs', ['actor_id'], unique=False)
    op.create_table('rate_limits',
    sa.Column('key', sa.String(length=64), nullable=False),
    sa.Column('count', sa.Integer(), nullable=False),
    sa.Column('reset_at', sa.DateTime(), nullable=False),
    sa.PrimaryKeyConstraint('key')
    )
    op.create_index(op.f('ix_rate_limits_reset_at'), 'rate_limits', ['reset_at'], unique=False)
    op.create_table('service_heartbeats',
    sa.Column('role', sa.String(length=16), nullable=False),
    sa.Column('updated_at', sa.DateTime(), nullable=False),
    sa.PrimaryKeyConstraint('role')
    )
    op.create_table('users',
    sa.Column('id', sa.String(length=36), nullable=False),
    sa.Column('username', sa.String(length=64), nullable=False),
    sa.Column('password_hash', sa.Text(), nullable=False),
    sa.Column('role', sa.String(length=16), nullable=False),
    sa.Column('status', sa.String(length=16), nullable=False),
    sa.Column('reviewed_by', sa.String(length=36), nullable=True),
    sa.Column('reviewed_at', sa.DateTime(), nullable=True),
    sa.Column('created_at', sa.DateTime(), nullable=False),
    sa.PrimaryKeyConstraint('id'),
    sa.UniqueConstraint('username')
    )
    op.create_table('jobs',
    sa.Column('id', sa.String(length=36), nullable=False),
    sa.Column('user_id', sa.String(length=36), nullable=False),
    sa.Column('title', sa.String(length=255), nullable=False),
    sa.Column('kind', sa.String(length=16), nullable=False),
    sa.Column('status', sa.String(length=16), nullable=False),
    sa.Column('stage', sa.String(length=32), nullable=True),
    sa.Column('stage_progress', sa.Float(), nullable=True),
    sa.Column('engine', sa.String(length=32), nullable=False),
    sa.Column('actual_device', sa.String(length=32), nullable=True),
    sa.Column('config_snapshot', sa.JSON(), nullable=False),
    sa.Column('warnings', sa.JSON(), nullable=False),
    sa.Column('error_code', sa.String(length=64), nullable=True),
    sa.Column('error_message', sa.Text(), nullable=True),
    sa.Column('attempt', sa.Integer(), nullable=False),
    sa.Column('lease_token', sa.String(length=36), nullable=True),
    sa.Column('lease_expires_at', sa.DateTime(), nullable=True),
    sa.Column('heartbeat_at', sa.DateTime(), nullable=True),
    sa.Column('cancel_requested', sa.Boolean(), nullable=False),
    sa.Column('delete_requested', sa.Boolean(), nullable=False),
    sa.Column('deleted_at', sa.DateTime(), nullable=True),
    sa.Column('created_at', sa.DateTime(), nullable=False),
    sa.Column('started_at', sa.DateTime(), nullable=True),
    sa.Column('completed_at', sa.DateTime(), nullable=True),
    sa.Column('expires_at', sa.DateTime(), nullable=True),
    sa.ForeignKeyConstraint(['user_id'], ['users.id'], ),
    sa.PrimaryKeyConstraint('id')
    )
    op.create_index(op.f('ix_jobs_expires_at'), 'jobs', ['expires_at'], unique=False)
    op.create_index(op.f('ix_jobs_lease_expires_at'), 'jobs', ['lease_expires_at'], unique=False)
    op.create_index('ix_jobs_owner_status', 'jobs', ['user_id', 'status'], unique=False)
    op.create_index(op.f('ix_jobs_status'), 'jobs', ['status'], unique=False)
    op.create_index(op.f('ix_jobs_user_id'), 'jobs', ['user_id'], unique=False)
    op.create_table('sessions',
    sa.Column('id', sa.String(length=36), nullable=False),
    sa.Column('token_hash', sa.String(length=64), nullable=False),
    sa.Column('user_id', sa.String(length=36), nullable=False),
    sa.Column('expires_at', sa.DateTime(), nullable=False),
    sa.Column('revoked_at', sa.DateTime(), nullable=True),
    sa.Column('created_at', sa.DateTime(), nullable=False),
    sa.ForeignKeyConstraint(['user_id'], ['users.id'], ),
    sa.PrimaryKeyConstraint('id'),
    sa.UniqueConstraint('token_hash')
    )
    op.create_index(op.f('ix_sessions_user_id'), 'sessions', ['user_id'], unique=False)
    op.create_table('settings',
    sa.Column('key', sa.String(length=64), nullable=False),
    sa.Column('value', sa.JSON(), nullable=False),
    sa.Column('updated_by', sa.String(length=36), nullable=True),
    sa.Column('updated_at', sa.DateTime(), nullable=False),
    sa.ForeignKeyConstraint(['updated_by'], ['users.id'], ),
    sa.PrimaryKeyConstraint('key')
    )
    op.create_table('queue_outbox',
    sa.Column('id', sa.String(length=36), nullable=False),
    sa.Column('job_id', sa.String(length=36), nullable=False),
    sa.Column('attempt', sa.Integer(), nullable=False),
    sa.Column('delivered_at', sa.DateTime(), nullable=True),
    sa.Column('next_retry_at', sa.DateTime(), nullable=False),
    sa.Column('tries', sa.Integer(), nullable=False),
    sa.Column('created_at', sa.DateTime(), nullable=False),
    sa.ForeignKeyConstraint(['job_id'], ['jobs.id'], ),
    sa.PrimaryKeyConstraint('id'),
    sa.UniqueConstraint('job_id', 'attempt')
    )
    op.create_index(op.f('ix_queue_outbox_job_id'), 'queue_outbox', ['job_id'], unique=False)
    op.create_index(op.f('ix_queue_outbox_next_retry_at'), 'queue_outbox', ['next_retry_at'], unique=False)
    op.create_table('tracks',
    sa.Column('id', sa.String(length=36), nullable=False),
    sa.Column('job_id', sa.String(length=36), nullable=False),
    sa.Column('label', sa.String(length=255), nullable=False),
    sa.Column('program', sa.Integer(), nullable=True),
    sa.Column('confidence', sa.Float(), nullable=True),
    sa.Column('label_source', sa.String(length=32), nullable=False),
    sa.Column('is_drum', sa.Boolean(), nullable=False),
    sa.Column('source_stem_ids', sa.JSON(), nullable=False),
    sa.ForeignKeyConstraint(['job_id'], ['jobs.id'], ),
    sa.PrimaryKeyConstraint('id')
    )
    op.create_index(op.f('ix_tracks_job_id'), 'tracks', ['job_id'], unique=False)
    op.create_table('artifacts',
    sa.Column('id', sa.String(length=36), nullable=False),
    sa.Column('job_id', sa.String(length=36), nullable=False),
    sa.Column('track_id', sa.String(length=36), nullable=True),
    sa.Column('kind', sa.String(length=32), nullable=False),
    sa.Column('label', sa.String(length=255), nullable=False),
    sa.Column('storage_key', sa.Text(), nullable=True),
    sa.Column('mime', sa.String(length=128), nullable=False),
    sa.Column('bytes', sa.Integer(), nullable=True),
    sa.Column('sha256', sa.String(length=64), nullable=True),
    sa.Column('status', sa.String(length=16), nullable=False),
    sa.Column('error', sa.Text(), nullable=True),
    sa.Column('attempt', sa.Integer(), nullable=False),
    sa.Column('created_at', sa.DateTime(), nullable=False),
    sa.ForeignKeyConstraint(['job_id'], ['jobs.id'], ),
    sa.PrimaryKeyConstraint('id')
    )
    op.create_index(op.f('ix_artifacts_job_id'), 'artifacts', ['job_id'], unique=False)

def downgrade():
    op.drop_index(op.f('ix_artifacts_job_id'), table_name='artifacts')
    op.drop_table('artifacts')
    op.drop_index(op.f('ix_tracks_job_id'), table_name='tracks')
    op.drop_table('tracks')
    op.drop_index(op.f('ix_queue_outbox_next_retry_at'), table_name='queue_outbox')
    op.drop_index(op.f('ix_queue_outbox_job_id'), table_name='queue_outbox')
    op.drop_table('queue_outbox')
    op.drop_table('settings')
    op.drop_index(op.f('ix_sessions_user_id'), table_name='sessions')
    op.drop_table('sessions')
    op.drop_index(op.f('ix_jobs_user_id'), table_name='jobs')
    op.drop_index(op.f('ix_jobs_status'), table_name='jobs')
    op.drop_index('ix_jobs_owner_status', table_name='jobs')
    op.drop_index(op.f('ix_jobs_lease_expires_at'), table_name='jobs')
    op.drop_index(op.f('ix_jobs_expires_at'), table_name='jobs')
    op.drop_table('jobs')
    op.drop_table('users')
    op.drop_table('service_heartbeats')
    op.drop_index(op.f('ix_rate_limits_reset_at'), table_name='rate_limits')
    op.drop_table('rate_limits')
    op.drop_index(op.f('ix_audit_logs_actor_id'), table_name='audit_logs')
    op.drop_table('audit_logs')
