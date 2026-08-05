<script setup lang="ts">
import { useNav } from "@/layout/hooks/useNav";
import LayNavMix from "../lay-sidebar/NavMix.vue";
import LaySidebarBreadCrumb from "../lay-sidebar/components/SidebarBreadCrumb.vue";
import LaySidebarTopCollapse from "../lay-sidebar/components/SidebarTopCollapse.vue";

const { layout, device, pureApp, toggleSideBar } = useNav();
</script>

<template>
  <div class="navbar bg-[#1d1f27] shadow-xs shadow-[rgba(0,0,0,0.3)]">
    <LaySidebarTopCollapse
      v-if="device === 'mobile'"
      class="hamburger-container"
      :is-active="pureApp.sidebar.opened"
      @toggleClick="toggleSideBar"
    />

    <LaySidebarBreadCrumb
      v-if="layout !== 'mix' && device !== 'mobile'"
      class="breadcrumb-container"
    />

    <LayNavMix v-if="layout === 'mix'" />
  </div>
</template>

<style lang="scss" scoped>
.navbar {
  width: 100%;
  height: 48px;
  overflow: hidden;
  color: #e0e0e0;

  .hamburger-container {
    float: left;
    height: 100%;
    line-height: 48px;
    cursor: pointer;
  }

  .breadcrumb-container {
    float: left;
    margin-left: 16px;
  }
}
</style>
