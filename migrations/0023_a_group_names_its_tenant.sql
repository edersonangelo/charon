-- A value the identity provider sends places somebody when it is the name of a
-- tenant that exists here. There is nothing to register par by par: the name
-- is the mapping. A value that names no tenant places nobody, and no tenant is
-- ever created from one.
drop table role_mapping;
