from alembic import context
from app.config import get_config
from app.db import engine
from app.models import Base

if context.is_offline_mode():
    context.configure(url=get_config().database_url, target_metadata=Base.metadata,
                      literal_binds=True, dialect_opts={'paramstyle': 'named'})
    with context.begin_transaction():
        context.run_migrations()
else:
    with engine().connect() as connection:
        context.configure(connection=connection, target_metadata=Base.metadata,
                          compare_type=True)
        with context.begin_transaction():
            context.run_migrations()
