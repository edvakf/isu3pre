#!/bin/bash

mysql -uisucon -Disucon -e 'DROP TABLE IF EXISTS public_memos; CREATE TABLE public_memos LIKE memos;'

