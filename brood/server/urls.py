"""URL сервера: админка и вход панели."""
from django.contrib import admin
from django.urls import path

from brood.server.base import views

urlpatterns = [
    path("admin/", admin.site.urls),
    path("auth/session", views.session),
    path("auth/login", views.login),
    path("auth/logout", views.logout),
    path("auth/llm", views.llm),
]
